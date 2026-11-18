// The HTTP layer: a raw TCP HTTP/1.1 server, no net/http. The suite's
// clients speak plain HTTP/1.1 with keep-alive and Content-Length (or
// chunked) bodies, so a hand-rolled parser over bufio is both the
// fastest path and small: one read loop per connection, one Write per
// response, and pre-built response bytes on the hot paths.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// Request is one parsed HTTP request.
type Request struct {
	Method      string
	Target      string
	Path        string
	Segments    []string
	Query       url.Values
	Headers     map[string]string
	Body        []byte
	JSON        any
	JSONBad     bool
	JSONPresent bool
	Token       string
	ReqID       string
	ConnClose   bool
}

// Responder writes one HTTP response on a connection.
type Responder struct {
	conn net.Conn
	buf  *bufio.Writer
}

// statusText is the reason phrase for the statuses this server sends.
func statusText(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 413:
		return "Content Too Large"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

const jsonContentType = "application/json; charset=utf-8"

var reqCounter uint64

// writeFull sends one complete response: status line, headers, body.
func (w *Responder) writeFull(req *Request, status int, contentType string, body []byte, extra map[string]string) {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(itoa(status))
	buf.WriteString(" ")
	buf.WriteString(statusText(status))
	buf.WriteString("\r\n")
	if contentType != "" {
		buf.WriteString("content-type: ")
		buf.WriteString(contentType)
		buf.WriteString("\r\n")
	}
	buf.WriteString("content-length: ")
	buf.WriteString(itoa(len(body)))
	buf.WriteString("\r\n")
	buf.WriteString("x-github-request-id: ")
	buf.WriteString(req.ReqID)
	buf.WriteString("\r\n")
	for key, value := range extra {
		buf.WriteString(key)
		buf.WriteString(": ")
		buf.WriteString(value)
		buf.WriteString("\r\n")
	}
	if req.ConnClose {
		buf.WriteString("connection: close\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	w.buf.Write(buf.Bytes())
	w.buf.Flush()
}

// writeJSON sends a JSON response whose body is already a string.
func (w *Responder) writeJSON(req *Request, status int, body string, extra map[string]string) {
	w.writeFull(req, status, jsonContentType, []byte(body), extra)
}

// writeObj sends a JSON response built from a value with encoding/json
// (HTML escaping off, like JSON.stringify).
func (w *Responder) writeObj(req *Request, status int, value any, extra map[string]string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	body := buf.Bytes()
	// Encoder appends a newline; drop it.
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	w.writeFull(req, status, jsonContentType, body, extra)
}

// writeErr sends {"message": "..."}.
func (w *Responder) writeErr(req *Request, status int, message string) {
	var buf bytes.Buffer
	buf.WriteString(`{"message":`)
	writeJSONString(&buf, message)
	buf.WriteString("}")
	w.writeFull(req, status, jsonContentType, buf.Bytes(), nil)
}

// writeEmpty sends a response with no body.
func (w *Responder) writeEmpty(req *Request, status int, extra map[string]string) {
	w.writeFull(req, status, "", nil, extra)
}

// writeBytes sends raw bytes with a given content type.
func (w *Responder) writeBytes(req *Request, status int, body []byte, contentType string) {
	w.writeFull(req, status, contentType, body, nil)
}

// ---------------------------------------------------------------------------
// JSON string escaping (JSON.stringify semantics)
// ---------------------------------------------------------------------------

// writeJSONString appends s as a quoted JSON string: control characters
// escaped, '"' and '\\' escaped, invalid UTF-8 replaced by U+FFFD, all
// other runes raw.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			buf.WriteString(`\ufffd`)
			s = s[1:]
			continue
		}
		s = s[size:]
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				const hexDigits = "0123456789abcdef"
				buf.WriteByte(hexDigits[byte(r)>>4])
				buf.WriteByte(hexDigits[byte(r)&0x0f])
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// jsonString returns s as a quoted JSON string.
func jsonString(s string) string {
	var buf bytes.Buffer
	writeJSONString(&buf, s)
	return buf.String()
}

// ---------------------------------------------------------------------------
// request parsing
// ---------------------------------------------------------------------------

const maxBodyBytes = 32 * 1024 * 1024

// bearerToken extracts the bearer token of a request, or "".
func bearerToken(headers map[string]string) string {
	header, ok := headers["authorization"]
	if !ok {
		return ""
	}
	if !strings.HasPrefix(header, "Bearer") {
		return ""
	}
	rest := header[len("Bearer"):]
	i := 0
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	if i == len(rest) {
		return ""
	}
	return rest[i:]
}

// parseSegments splits a path into decoded segments.
func parseSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if decoded, err := url.PathUnescape(part); err == nil {
			part = decoded
		}
		out = append(out, part)
	}
	return out
}

// readRequest parses one request from br, writing the 100-continue
// interim response on conn before reading a body the client holds back.
// It returns io.EOF when the connection closed cleanly before a
// request line.
func readRequest(br *bufio.Reader, conn net.Conn) (*Request, error) {
	// Skip stray blank lines between keep-alive requests.
	var line string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(l, "\r\n")
		if line != "" {
			break
		}
	}
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		return nil, io.ErrUnexpectedEOF
	}
	req := &Request{
		Method:  fields[0],
		Target:  fields[1],
		Headers: map[string]string{},
	}
	target := fields[1]
	path := target
	query := ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		path = target[:i]
		query = target[i+1:]
	}
	req.Path = path
	req.Segments = parseSegments(path)
	if values, err := url.ParseQuery(query); err == nil {
		req.Query = values
	} else {
		req.Query = url.Values{}
	}

	// Headers.
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		if i := strings.IndexByte(l, ':'); i > 0 {
			name := strings.ToLower(strings.TrimSpace(l[:i]))
			value := strings.TrimSpace(l[i+1:])
			req.Headers[name] = value
		}
	}
	req.Token = bearerToken(req.Headers)
	if conn := req.Headers["connection"]; strings.EqualFold(conn, "close") {
		req.ConnClose = true
	}

	// 100-continue: acknowledge before reading the body, or the client
	// never sends it.
	if expect, ok := req.Headers["expect"]; ok && strings.EqualFold(expect, "100-continue") {
		_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}

	// Body.
	if req.Method == "POST" || req.Method == "PUT" || req.Method == "PATCH" || req.Method == "DELETE" {
		body, err := readBody(br, req.Headers)
		if err != nil {
			return nil, err
		}
		req.Body = body
		if len(body) > 0 && strings.Contains(req.Headers["content-type"], "application/json") {
			req.JSONPresent = true
			var parsed any
			dec := json.NewDecoder(bytes.NewReader(body))
			if err := dec.Decode(&parsed); err != nil {
				req.JSONBad = true
			} else {
				req.JSON = parsed
			}
		}
	}
	req.ReqID = "req-" + strconv.FormatUint(atomic.AddUint64(&reqCounter, 1), 16)
	return req, nil
}

// readBody reads a request body by Content-Length or chunked encoding.
func readBody(br *bufio.Reader, headers map[string]string) ([]byte, error) {
	if te, ok := headers["transfer-encoding"]; ok && strings.Contains(strings.ToLower(te), "chunked") {
		var out bytes.Buffer
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return nil, err
			}
			sizeLine = strings.TrimRight(sizeLine, "\r\n")
			if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
				sizeLine = sizeLine[:i]
			}
			size, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
			if err != nil {
				return nil, err
			}
			if size == 0 {
				// Trailers until a blank line.
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						return nil, err
					}
					if strings.TrimRight(l, "\r\n") == "" {
						break
					}
				}
				return out.Bytes(), nil
			}
			if out.Len()+int(size) > maxBodyBytes {
				return nil, io.ErrShortBuffer
			}
			if _, err := io.CopyN(&out, br, size); err != nil {
				return nil, err
			}
			// Trailing CRLF after the chunk.
			if _, err := br.ReadString('\n'); err != nil {
				return nil, err
			}
		}
	}
	lengthStr := headers["content-length"]
	if lengthStr == "" {
		return nil, nil
	}
	length, err := strconv.Atoi(strings.TrimSpace(lengthStr))
	if err != nil || length < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	if length == 0 {
		return nil, nil
	}
	if length > maxBodyBytes {
		return nil, io.ErrShortBuffer
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// connection loop
// ---------------------------------------------------------------------------

// Handler serves one request.
type Handler func(w *Responder, req *Request)

func handleConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	bw := bufio.NewWriterSize(conn, 65536)
	w := &Responder{conn: conn, buf: bw}
	for {
		req, err := readRequest(br, conn)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			// A body that is too large, or a malformed request: close.
			return
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					w.writeErr(req, 500, "Internal Server Error")
				}
			}()
			handler(w, req)
		}()
		if req.ConnClose {
			return
		}
	}
}

// serve accepts connections forever.
func serve(ln net.Listener, handler Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, handler)
	}
}
