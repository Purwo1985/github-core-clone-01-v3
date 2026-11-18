// Git object model: hashing, tree/commit/tag codecs, pkt-line framing and
// packfile read/write. Objects are stored exactly as these functions
// produce them, so a hash recomputed over stored bytes always matches
// the id.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

const zeroOID = "0000000000000000000000000000000000000000"

// ObjType is a git object type name.
type ObjType string

const (
	objCommit ObjType = "commit"
	objTree   ObjType = "tree"
	objBlob   ObjType = "blob"
	objTag    ObjType = "tag"
)

var typeCode = map[ObjType]byte{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}
var codeType = map[byte]ObjType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

// GitObject is one stored object: its type and raw bytes.
type GitObject struct {
	Type ObjType
	Data []byte
}

// hashObject returns the SHA-1 over "<type> <size>\0" + data.
func hashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func isOID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// trees
// ---------------------------------------------------------------------------

// TreeEntry is one entry of a tree object.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses a tree's raw bytes into entries.
func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], ' ')
		if space < 0 {
			break
		}
		space += off
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		mode := string(data[off:space])
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		off = nul + 21
	}
	return entries
}

// treeSortKey is git's tree sort: names compare with a trailing '/'
// for directories.
func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// buildTree serialises entries in canonical git order.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, entry := range sorted {
		fmt.Fprintf(&buf, "%s %s\x00", entry.Mode, entry.Name)
		raw, _ := hex.DecodeString(entry.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// emptyTreeOID is the empty tree's id.
var emptyTreeOID = hashObject(objTree, nil)

// ---------------------------------------------------------------------------
// commits and tags
// ---------------------------------------------------------------------------

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) CommitView {
	text := string(data)
	blank := -1
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '\n' && text[i+1] == '\n' {
			blank = i
			break
		}
	}
	var head, message string
	if blank == -1 {
		head = text
	} else {
		head = text[:blank]
		message = text[blank+2:]
	}
	view := CommitView{}
	for _, line := range splitLines(head) {
		switch {
		case len(line) > 5 && line[:5] == "tree ":
			view.Tree = trimSpace(line[5:])
		case len(line) > 7 && line[:7] == "parent ":
			view.Parents = append(view.Parents, trimSpace(line[7:]))
		case len(line) > 7 && line[:7] == "author ":
			view.Author = line[7:]
		case len(line) > 10 && line[:10] == "committer ":
			view.Committer = line[10:]
		}
	}
	view.Message = message
	return view
}

// TagView is a parsed tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) TagView {
	text := string(data)
	blank := -1
	for i := 0; i+1 < len(text); i++ {
		if text[i] == '\n' && text[i+1] == '\n' {
			blank = i
			break
		}
	}
	var head, message string
	if blank == -1 {
		head = text
	} else {
		head = text[:blank]
		message = text[blank+2:]
	}
	view := TagView{}
	for _, line := range splitLines(head) {
		switch {
		case len(line) > 7 && line[:7] == "object ":
			view.Object = trimSpace(line[7:])
		case len(line) > 5 && line[:5] == "type ":
			view.Type = trimSpace(line[5:])
		case len(line) > 4 && line[:4] == "tag ":
			view.Tag = trimSpace(line[4:])
		case len(line) > 7 && line[:7] == "tagger ":
			view.Tagger = line[7:]
		}
	}
	view.Message = message
	return view
}

// buildCommit serialises a commit object.
func buildCommit(tree string, parents []string, message, author, committer string, timestamp int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&buf, "parent %s\n", parent)
	}
	fmt.Fprintf(&buf, "author %s %d +0000\n", author, timestamp)
	fmt.Fprintf(&buf, "committer %s %d +0000\n", committer, timestamp)
	buf.WriteString("\n")
	buf.WriteString(message)
	return buf.Bytes()
}

// buildTag serialises a tag object.
func buildTag(object, typ, tag, message, tagger string, timestamp int64) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "object %s\ntype %s\ntag %s\ntagger %s %d +0000\n\n%s", object, typ, tag, tagger, timestamp, message)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

const pktLineMax = 65520

func pktLine(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	return append(out, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLines frames items: byte payloads, "flush" or "delim".
func pktLines(items []any) []byte {
	var buf bytes.Buffer
	for _, item := range items {
		switch v := item.(type) {
		case string:
			if v == "flush" {
				buf.Write(pktFlush)
			} else if v == "delim" {
				buf.Write(pktDelim)
			} else {
				buf.Write(pktLine([]byte(v)))
			}
		case []byte:
			buf.Write(pktLine(v))
		}
	}
	return buf.Bytes()
}

// Packet is one decoded pkt-line.
type Packet struct {
	Kind    string // "data", "flush", "delim", "response-end"
	Payload []byte
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodePktLines decodes a whole buffer of pkt-lines. Trailing raw bytes
// (a pack after the command list) are returned separately.
func decodePktLines(data []byte) ([]Packet, []byte, error) {
	var packets []Packet
	off := 0
	for off < len(data) {
		if off+4 > len(data) {
			return nil, nil, errors.New("truncated pkt-line header")
		}
		head := string(data[off : off+4])
		if !isHex4(head) {
			break // raw bytes follow (a packfile)
		}
		length64, _ := strconv.ParseInt(head, 16, 32)
		length := int(length64)
		if length == 0 {
			packets = append(packets, Packet{Kind: "flush"})
			off += 4
		} else if length == 1 {
			packets = append(packets, Packet{Kind: "delim"})
			off += 4
		} else if length == 2 {
			packets = append(packets, Packet{Kind: "response-end"})
			off += 4
		} else if length == 3 || length > pktLineMax {
			return nil, nil, fmt.Errorf("invalid pkt-line length %d", length)
		} else {
			if off+length > len(data) {
				return nil, nil, errors.New("truncated pkt-line")
			}
			payload := make([]byte, length-4)
			copy(payload, data[off+4:off+length])
			packets = append(packets, Packet{Kind: "data", Payload: payload})
			off += length
		}
	}
	return packets, data[off:], nil
}

// packetTexts returns the text of every data packet, in order.
func packetTexts(packets []Packet) []string {
	out := make([]string, 0, len(packets))
	for _, packet := range packets {
		if packet.Kind == "data" {
			out = append(out, string(packet.Payload))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// deltas and packfiles
// ---------------------------------------------------------------------------

func readVarint(buffer []byte, start int) (int, int, error) {
	value := 0
	shift := uint(0)
	off := start
	for {
		if off >= len(buffer) {
			return 0, 0, errors.New("delta varint truncated")
		}
		b := buffer[off]
		off++
		value += int(b&0x7f) * (1 << shift)
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return value, off, nil
}

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	srcSize, next, err := readVarint(delta, off)
	if err != nil {
		return nil, err
	}
	off = next
	tgtSize, next, err := readVarint(delta, off)
	if err != nil {
		return nil, err
	}
	off = next
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff := 0
			copySize := 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copyOff += int(delta[off]) * (1 << shift)
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					copySize += int(delta[off]) * (1 << shift)
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || written+copySize > tgtSize {
				return nil, errors.New("delta copy out of range")
			}
			copy(out[written:written+copySize], base[copyOff:copyOff+copySize])
			written += copySize
		} else if op != 0 {
			length := int(op & 0x7f)
			if off+length > len(delta) || written+length > tgtSize {
				return nil, errors.New("delta insert out of range")
			}
			copy(out[written:written+length], delta[off:off+length])
			off += length
			written += length
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != tgtSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

func readPackSize(buffer []byte, start int) (size int, next int, first byte, err error) {
	if start >= len(buffer) {
		return 0, 0, 0, errors.New("pack entry has no size header")
	}
	b := buffer[start]
	first = b
	size = int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for b&0x80 != 0 {
		if off >= len(buffer) {
			return 0, 0, 0, errors.New("pack size varint truncated")
		}
		b = buffer[off]
		off++
		size += int(b&0x7f) * (1 << shift)
		shift += 7
	}
	return size, off, first, nil
}

func readOfsBase(buffer []byte, start int) (int, int, error) {
	b := buffer[start]
	off := start + 1
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buffer) {
			return 0, 0, errors.New("ofs-delta base truncated")
		}
		b = buffer[off]
		off++
		value = (value+1)*128 + int(b&0x7f)
	}
	return value, off, nil
}

// inflateAt inflates one zlib stream starting at offset and reports the
// data plus the offset just past the stream. It works because flate reads
// a *bytes.Reader one byte at a time (io.ByteReader), so nothing past the
// stream's own bytes is consumed.
func inflateAt(buffer []byte, offset, end int) ([]byte, int, error) {
	br := bytes.NewReader(buffer[offset:end])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		zr.Close()
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	pos, _ := br.Seek(0, io.SeekCurrent)
	return data, offset + int(pos), nil
}

// PackRead is a parsed packfile.
type PackRead struct {
	Version    int
	Objects    map[string]GitObject
	DeltaCount int
}

type rawPackEntry struct {
	offset    int
	typeCode  byte
	size      int
	data      []byte
	baseOff   int
	baseOID   string
	hasBase   bool
	isOfs     bool
	isRef     bool
	resolved  bool
	resType   ObjType
	resData   []byte
	resOID    string
	resolving bool
}

// readPack parses a packfile (v2/v3), resolving OFS_DELTA and REF_DELTA
// entries (thin packs resolve their bases against resolveBase).
func readPack(pack []byte, resolveBase func(oid string) (GitObject, bool)) (PackRead, error) {
	if len(pack) < 32 {
		return PackRead{}, errors.New("pack too short")
	}
	if string(pack[0:4]) != "PACK" {
		return PackRead{}, errors.New("pack does not start with PACK")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return PackRead{}, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	h := sha1.New()
	h.Write(pack[:trailerStart])
	if hex.EncodeToString(h.Sum(nil)) != trailer {
		return PackRead{}, errors.New("pack trailer checksum mismatch")
	}

	raw := make([]*rawPackEntry, 0, count)
	byOffset := map[int]*rawPackEntry{}
	off := 12
	for i := 0; i < count; i++ {
		entryOffset := off
		size, next, first, err := readPackSize(pack, off)
		if err != nil {
			return PackRead{}, err
		}
		off = next
		tc := (first >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 {
			base, next, err := readOfsBase(pack, off)
			if err != nil {
				return PackRead{}, err
			}
			off = next
			entry.baseOff = entryOffset - base
			entry.hasBase = true
			entry.isOfs = true
		} else if tc == 7 {
			if off+20 > trailerStart {
				return PackRead{}, errors.New("ref-delta base truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
			entry.hasBase = true
			entry.isRef = true
		} else if _, ok := codeType[tc]; !ok {
			return PackRead{}, fmt.Errorf("unknown pack type code %d", tc)
		}
		inflated, nextOff, err := inflateAt(pack, off, trailerStart)
		if err != nil {
			return PackRead{}, err
		}
		if len(inflated) != size {
			return PackRead{}, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		off = nextOff
		raw = append(raw, entry)
		byOffset[entry.offset] = entry
	}
	if off != trailerStart {
		return PackRead{}, errors.New("pack has stray bytes before trailer")
	}

	objects := make(map[string]GitObject, count)
	resolvedByOID := map[string]*rawPackEntry{}
	deltaCount := 0

	var resolve func(entry *rawPackEntry) (ObjType, []byte, string, error)
	resolve = func(entry *rawPackEntry) (ObjType, []byte, string, error) {
		if entry.resolved {
			return entry.resType, entry.resData, entry.resOID, nil
		}
		if entry.resolving {
			return "", nil, "", errors.New("delta cycle")
		}
		entry.resolving = true
		var typ ObjType
		var data []byte
		if entry.isOfs {
			baseEntry, ok := byOffset[entry.baseOff]
			if !ok {
				return "", nil, "", errors.New("ofs-delta base not in pack")
			}
			baseType, baseData, _, err := resolve(baseEntry)
			if err != nil {
				return "", nil, "", err
			}
			typ = baseType
			data, err = applyDelta(baseData, entry.data)
			if err != nil {
				return "", nil, "", err
			}
		} else if entry.isRef {
			if baseEntry, ok := resolvedByOID[entry.baseOID]; ok {
				baseType, baseData, _, err := resolve(baseEntry)
				if err != nil {
					return "", nil, "", err
				}
				typ = baseType
				applied, err := applyDelta(baseData, entry.data)
				if err != nil {
					return "", nil, "", err
				}
				data = applied
			} else {
				external, ok := resolveBase(entry.baseOID)
				if !ok {
					return "", nil, "", errors.New("ref-delta base not available")
				}
				typ = external.Type
				applied, err := applyDelta(external.Data, entry.data)
				if err != nil {
					return "", nil, "", err
				}
				data = applied
			}
		} else {
			typ = codeType[entry.typeCode]
			data = entry.data
		}
		oid := hashObject(typ, data)
		entry.resType = typ
		entry.resData = data
		entry.resOID = oid
		entry.resolved = true
		entry.resolving = false
		resolvedByOID[oid] = entry
		return typ, data, oid, nil
	}

	for _, entry := range raw {
		typ, data, oid, err := resolve(entry)
		if err != nil {
			return PackRead{}, err
		}
		if entry.isOfs || entry.isRef {
			deltaCount++
		}
		if _, ok := objects[oid]; !ok {
			objects[oid] = GitObject{Type: typ, Data: data}
		}
	}
	return PackRead{Version: version, Objects: objects, DeltaCount: deltaCount}, nil
}

func packObjectHeader(typ ObjType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	remaining := size
	first := byte(code<<4) | byte(remaining&0x0f)
	remaining = remaining / 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining = remaining / 128
	}
	out = append(out, first)
	return out
}

func deflateLevel1(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, 1)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writePack builds a packfile from plain objects (no deltas), with a
// SHA-1 trailer.
func writePack(objects []GitObject) []byte {
	var body bytes.Buffer
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	body.Write([]byte{byte(len(objects) >> 24), byte(len(objects) >> 16), byte(len(objects) >> 8), byte(len(objects))})
	for _, object := range objects {
		body.Write(packObjectHeader(object.Type, len(object.Data)))
		body.Write(deflateLevel1(object.Data))
	}
	raw := body.Bytes()
	h := sha1.New()
	h.Write(raw)
	return append(raw, h.Sum(nil)...)
}

// packChecksumOk verifies a pack's trailer checksum without parsing it.
func packChecksumOk(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	actual := hex.EncodeToString(pack[len(pack)-20:])
	h := sha1.New()
	h.Write(pack[:len(pack)-20])
	return hex.EncodeToString(h.Sum(nil)) == actual
}

// splitLines splits on '\n' without keeping the terminator.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// trimSpace trims ASCII spaces and tabs from both ends.
func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// itoa converts a small int to decimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
