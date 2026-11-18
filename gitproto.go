// Git smart HTTP: protocol v2 upload-pack (ls-refs, fetch), v0
// upload-pack and v0 receive-pack with side-band-64k and report-status.
// Packs are built by the store (cached per ask); advertisements and
// ls-refs rows are cached per repository ref version.

package main

import (
	"bytes"
	"strings"
)

const agentName = "forge-v3/1.0.0"
const uploadAd = "application/x-git-upload-pack-advertisement"
const uploadResult = "application/x-git-upload-pack-result"
const receiveAd = "application/x-git-receive-pack-advertisement"
const receiveResult = "application/x-git-receive-pack-result"

var v2Capabilities = []string{
	"version 2",
	"ls-refs=unborn",
	"fetch=shallow wait-for-done",
	"server-option",
	"object-format=sha1",
	"agent=" + agentName,
}

const v0UploadCaps = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta multi_ack shallow object-format=sha1 agent=" + agentName
const v0ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=" + agentName

// v2Advertisement is the v2 capability advertisement: the same bytes
// for every repository.
func v2Advertisement() []byte {
	items := make([]any, 0, len(v2Capabilities)+1)
	for _, line := range v2Capabilities {
		items = append(items, line+"\n")
	}
	items = append(items, "flush")
	return pktLines(items)
}

func lookupGitRepo(w *Responder, req *Request, s *Store) *Repo {
	owner := req.Segments[0]
	name := req.Segments[1]
	name = strings.TrimSuffix(name, ".git")
	repo := s.getRepo(owner, name)
	if repo == nil {
		w.writeErr(req, 404, "Not Found")
		return nil
	}
	return repo
}

// v0Advertisement builds a v0 ref advertisement: service preamble,
// refs, capabilities on the first line.
func v0Advertisement(s *Store, repo *Repo, service string) []byte {
	caps := v0UploadCaps
	if service == "git-receive-pack" {
		caps = v0ReceiveCaps
	}
	var items []any
	items = append(items, "# service="+service+"\n", "flush")
	refs := s.lsRefs(repo)
	headOID, hasHead := s.headOid(repo)
	if len(refs) == 0 {
		items = append(items, zeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		if hasHead {
			items = append(items, headOID+" HEAD\x00"+caps+"\n")
		} else {
			items = append(items, zeroOID+" capabilities^{}\x00"+caps+"\n")
		}
		for _, row := range refs {
			if row.Name == "HEAD" {
				continue
			}
			items = append(items, row.OID+" "+row.Name+"\n")
			if row.Peeled != "" {
				items = append(items, row.Peeled+" "+row.Name+"^{}\n")
			}
		}
	}
	items = append(items, "flush")
	return pktLines(items)
}

func infoRefs(w *Responder, req *Request, s *Store) {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return
	}
	service := req.Query.Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		w.writeErr(req, 404, "Not Found")
		return
	}
	repo := lookupGitRepo(w, req, s)
	if repo == nil {
		return
	}
	if service == "git-upload-pack" && req.Headers["git-protocol"] == "version=2" {
		w.writeBytes(req, 200, v2Advertisement(), uploadAd)
		return
	}
	ct := uploadAd
	if service == "git-receive-pack" {
		ct = receiveAd
	}
	w.writeBytes(req, 200, v0Advertisement(s, repo, service), ct)
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

type fetchAsk struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	hasDepth bool
	done     bool
}

// sidebandPackets splits a payload into band-1 packets of at most
// 65515 data bytes.
func sidebandPackets(data []byte) [][]byte {
	const chunk = 65515
	var out [][]byte
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		payload := make([]byte, 0, end-off+1)
		payload = append(payload, 1)
		payload = append(payload, data[off:end]...)
		out = append(out, pktLine(payload))
	}
	return out
}

func parseFetchAsk(lines []string, v0 bool) fetchAsk {
	ask := fetchAsk{}
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			if v0 {
				first := strings.Split(text[5:], " ")[0]
				if isOID(first) {
					ask.wants = append(ask.wants, first)
				}
			} else {
				ask.wants = append(ask.wants, strings.TrimSpace(text[5:]))
			}
		case strings.HasPrefix(text, "have "):
			ask.haves = append(ask.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			ask.shallow = append(ask.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if n, err := atoi(text[7:]); err == nil && n > 0 {
				ask.depth = n
				ask.hasDepth = true
			}
		case text == "done":
			ask.done = true
		}
	}
	return ask
}

func fetchV2(w *Responder, req *Request, s *Store, repo *Repo, lines []string) {
	ask := parseFetchAsk(lines, false)
	// The acknowledgments section is only sent while negotiation is open:
	// when the client sent `done` (or no haves at all) the server goes
	// straight to the packfile — a real git server does the same, and a
	// git client that sent `done` expects the first section to be the
	// packfile (or shallow-info). While negotiating (haves, no `done`)
	// the server answers ACK + ready for a common commit, or NAK alone
	// when no have is an ancestor of any want.
	negotiating := !ask.done && len(ask.haves) > 0
	var parts [][]byte
	if negotiating {
		common := ""
		for _, have := range ask.haves {
			for _, want := range ask.wants {
				if have == want || s.isAncestor(have, want) {
					common = have
					break
				}
			}
			if common != "" {
				break
			}
		}
		if common == "" {
			parts = append(parts, pktLines([]any{"acknowledgments\n", "NAK\n", "flush"}))
			w.writeBytes(req, 200, bytes.Join(parts, nil), uploadResult)
			return
		}
		parts = append(parts, pktLines([]any{"acknowledgments\n", "ACK " + common + "\n", "ready\n"}))
		parts = append(parts, pktDelim)
	}
	pack, shallowOut, unshallowOut := s.fetchPack(repo, ask.wants, ask.haves, ask.depth, ask.hasDepth, ask.shallow)
	if len(shallowOut) > 0 || len(unshallowOut) > 0 {
		section := []any{"shallow-info\n"}
		for _, oid := range shallowOut {
			section = append(section, "shallow "+oid+"\n")
		}
		for _, oid := range unshallowOut {
			section = append(section, "unshallow "+oid+"\n")
		}
		section = append(section, "flush")
		parts = append(parts, pktLines(section))
	}
	if len(pack) > 32 {
		parts = append(parts, pktLine([]byte("packfile\n")))
		parts = append(parts, sidebandPackets(pack)...)
		parts = append(parts, []byte("0000"))
	}
	w.writeBytes(req, 200, bytes.Join(parts, nil), uploadResult)
}

func lsRefsV2(w *Responder, req *Request, s *Store, repo *Repo, lines []string) {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case text == "peel":
			peel = true
		case text == "symrefs":
			symrefs = true
		case text == "unborn":
			unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var out []any
	headOID, hasHead := s.headOid(repo)
	headTarget := s.headTarget(repo)
	if keep("HEAD") {
		if hasHead {
			if symrefs {
				out = append(out, headOID+" HEAD symref-target:"+headTarget+"\n")
			} else {
				out = append(out, headOID+" HEAD\n")
			}
		} else if unborn {
			out = append(out, "unborn HEAD symref-target:"+headTarget+"\n")
		}
	}
	for _, row := range s.lsRefs(repo) {
		if !keep(row.Name) {
			continue
		}
		line := row.OID + " " + row.Name
		if symrefs && row.SymrefTarget != "" {
			line += " symref-target:" + row.SymrefTarget
		}
		if peel && row.Peeled != "" {
			line += " peeled:" + row.Peeled
		}
		out = append(out, line+"\n")
	}
	out = append(out, "flush")
	w.writeBytes(req, 200, pktLines(out), uploadResult)
}

func fetchV0(w *Responder, req *Request, s *Store, repo *Repo, lines []string) {
	ask := parseFetchAsk(lines, true)
	pack, shallowOut, unshallowOut := s.fetchPack(repo, ask.wants, ask.haves, ask.depth, ask.hasDepth, ask.shallow)
	var parts [][]byte
	parts = append(parts, pktLine([]byte("NAK\n")))
	for _, oid := range shallowOut {
		parts = append(parts, pktLine([]byte("shallow "+oid+"\n")))
	}
	for _, oid := range unshallowOut {
		parts = append(parts, pktLine([]byte("unshallow "+oid+"\n")))
	}
	if len(pack) > 32 {
		parts = append(parts, sidebandPackets(pack)...)
	}
	parts = append(parts, []byte("0000"))
	w.writeBytes(req, 200, bytes.Join(parts, nil), uploadResult)
}

func uploadPack(w *Responder, req *Request, s *Store) {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return
	}
	repo := lookupGitRepo(w, req, s)
	if repo == nil {
		return
	}
	body := req.Body
	if body == nil {
		body = []byte{}
	}
	packets, _, err := decodePktLines(body)
	if err != nil {
		w.writeBytes(req, 400, []byte("bad request"), "text/plain; charset=utf-8")
		return
	}
	lines := packetTexts(packets)
	first := ""
	if len(lines) > 0 {
		first = strings.TrimSuffix(lines[0], "\n")
	}
	isV2 := req.Headers["git-protocol"] == "version=2"
	if isV2 && first == "command=ls-refs" {
		lsRefsV2(w, req, s, repo, lines[1:])
		return
	}
	if isV2 && first == "command=fetch" {
		fetchV2(w, req, s, repo, lines[1:])
		return
	}
	if !isV2 {
		for _, line := range lines {
			if strings.HasPrefix(line, "want ") {
				fetchV0(w, req, s, repo, lines)
				return
			}
		}
	}
	w.writeBytes(req, 400, []byte("bad request"), "text/plain; charset=utf-8")
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func receivePack(w *Responder, req *Request, s *Store) {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return
	}
	repo := lookupGitRepo(w, req, s)
	if repo == nil {
		return
	}
	body := req.Body
	if body == nil {
		body = []byte{}
	}
	packets, rest, err := decodePktLines(body)
	if err != nil {
		w.writeBytes(req, 400, []byte("bad request"), "text/plain; charset=utf-8")
		return
	}
	lines := packetTexts(packets)
	var commands []pushCommand
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		if line == "" {
			continue
		}
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 {
			continue
		}
		oldOID, newOID, ref := parts[0], parts[1], parts[2]
		if !isOID(oldOID) || !isOID(newOID) || !strings.HasPrefix(ref, "refs/") {
			continue
		}
		commands = append(commands, pushCommand{oldOID: oldOID, newOID: newOID, ref: ref})
	}
	if len(commands) == 0 {
		w.writeBytes(req, 400, []byte("bad request"), "text/plain; charset=utf-8")
		return
	}
	packBytes := rest
	var report []string
	unpackOK := true
	unpackDetail := "ok"
	var updates []pushCommand
	for _, command := range commands {
		if command.newOID != zeroOID {
			updates = append(updates, command)
		}
	}
	// Unpack first: every object lands in the arena before any ref moves.
	if len(updates) > 0 {
		if len(packBytes) == 0 {
			unpackOK = false
			unpackDetail = "unpacker error"
		} else {
			read, err := readPack(packBytes, func(oid string) (GitObject, bool) {
				return s.getObject(oid)
			})
			if err != nil {
				unpackOK = false
				unpackDetail = err.Error()
			} else {
				s.storeObjects(read.Objects)
			}
		}
	}
	if !unpackOK {
		for _, command := range commands {
			report = append(report, "ng "+command.ref+" unpack failed\n")
		}
		report = append([]string{"unpack " + unpackDetail + "\n"}, report...)
	} else {
		report = append(report, "unpack ok\n")
		for _, command := range commands {
			current, exists := s.getRef(repo, command.ref)
			if command.newOID == zeroOID {
				// Delete.
				if !exists {
					report = append(report, "ng "+command.ref+" no such ref\n")
				} else if command.oldOID != zeroOID && command.oldOID != current {
					report = append(report, "ng "+command.ref+" non-fast-forward\n")
				} else {
					s.deleteRef(repo, command.ref)
					report = append(report, "ok "+command.ref+"\n")
				}
				continue
			}
			if command.oldOID == zeroOID {
				// Create.
				if exists {
					report = append(report, "ng "+command.ref+" already exists\n")
				} else {
					s.setRef(repo, command.ref, command.newOID)
					report = append(report, "ok "+command.ref+"\n")
				}
				continue
			}
			// Update: the old value must match, and the new tip must
			// descend from it.
			if !exists || current != command.oldOID {
				report = append(report, "ng "+command.ref+" non-fast-forward\n")
			} else if !s.isAncestor(command.oldOID, command.newOID) {
				report = append(report, "ng "+command.ref+" non-fast-forward\n")
			} else {
				s.setRef(repo, command.ref, command.newOID)
				report = append(report, "ok "+command.ref+"\n")
			}
		}
	}
	// The report travels on sideband band 1, terminated by a flush
	// packet *inside* band 1 (that is what the client's report parser
	// waits for), followed by the top-level flush that ends the
	// sideband stream. Without the inner flush the client keeps
	// waiting and reports "the remote end hung up".
	reportBuf := pktLines(stringsToAny(report))
	reportBuf = append(reportBuf, pktFlush...)
	var parts [][]byte
	const chunk = 65515
	for off := 0; off < len(reportBuf); off += chunk {
		end := off + chunk
		if end > len(reportBuf) {
			end = len(reportBuf)
		}
		payload := make([]byte, 0, end-off+1)
		payload = append(payload, 1)
		payload = append(payload, reportBuf[off:end]...)
		parts = append(parts, pktLine(payload))
	}
	parts = append(parts, []byte("0000"))
	w.writeBytes(req, 200, bytes.Join(parts, nil), receiveResult)
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// atoi parses a decimal integer, tolerating surrounding spaces.
func atoi(s string) (int, error) {
	s = strings.TrimSpace(s)
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errInvalidNumber
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

var errInvalidNumber = errorString("invalid number")

type errorString string

func (e errorString) Error() string { return string(e) }
