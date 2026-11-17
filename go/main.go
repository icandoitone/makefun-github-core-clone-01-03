// Forge core: a single-process, in-memory, GitHub-compatible forge.
// Hand-rolled HTTP/1.1 over raw TCP sockets; standard library only.
package main

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const fixedTS = "2025-01-01T00:00:00Z"

var token string
var store = NewStore()

var reqSeq atomic.Int64

// ---------- connection / HTTP parsing ----------

type request struct {
	method  string
	path    string // decoded path, no query
	rawPath string
	query   map[string]string
	headers map[string]string
	body    []byte
	host    string
}

type conn struct {
	c    net.Conn
	rb   []byte // read buffer
	rp   int    // read position
	tmp  [65536]byte
	keep bool
}

func (cn *conn) fill() error {
	if cn.rp > 0 {
		copy(cn.rb, cn.rb[cn.rp:])
		cn.rb = cn.rb[:len(cn.rb)-cn.rp]
		cn.rp = 0
	}
	n, err := cn.c.Read(cn.tmp[:])
	if n > 0 {
		cn.rb = append(cn.rb, cn.tmp[:n]...)
	}
	return err
}

// readLine returns one line without the trailing \n (and \r), or ""+err.
func (cn *conn) readLine() (string, error) {
	for {
		for i := cn.rp; i < len(cn.rb); i++ {
			if cn.rb[i] == '\n' {
				s := string(cn.rb[cn.rp:i])
				cn.rp = i + 1
				return strings.TrimSuffix(s, "\r"), nil
			}
		}
		if err := cn.fill(); err != nil {
			return "", err
		}
	}
}

func (cn *conn) readN(n int) ([]byte, error) {
	for len(cn.rb)-cn.rp < n {
		if err := cn.fill(); err != nil {
			return nil, err
		}
	}
	out := cn.rb[cn.rp : cn.rp+n]
	cn.rp += n
	return out, nil
}

// readRequest parses one HTTP/1.1 request. Returns nil, nil on clean EOF.
func (cn *conn) readRequest() (*request, error) {
	line, err := cn.readLine()
	if err != nil {
		return nil, nil
	}
	if line == "" {
		// stray blank line before request; read one more
		line, err = cn.readLine()
		if err != nil {
			return nil, nil
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("bad request line")
	}
	rq := &request{method: parts[0], headers: map[string]string{}, query: map[string]string{}}
	target := parts[1]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		rq.rawPath = target[:i]
		parseQuery(target[i+1:], rq.query)
	} else {
		rq.rawPath = target
	}
	rq.path = unescape(rq.rawPath)
	for {
		h, err := cn.readLine()
		if err != nil {
			return nil, err
		}
		if h == "" {
			break
		}
		if i := strings.IndexByte(h, ':'); i >= 0 {
			rq.headers[strings.ToLower(strings.TrimSpace(h[:i]))] = strings.TrimSpace(h[i+1:])
		}
	}
	rq.host = rq.headers["host"]
	if cl := rq.headers["content-length"]; cl != "" {
		n, _ := strconv.Atoi(cl)
		if n < 0 || n > 64<<20 {
			return nil, fmt.Errorf("bad content-length")
		}
		if n > 0 {
			b, err := cn.readN(n)
			if err != nil {
				return nil, err
			}
			rq.body = b
		}
	} else if strings.Contains(rq.headers["transfer-encoding"], "chunked") {
		for {
			ln, err := cn.readLine()
			if err != nil {
				return nil, err
			}
			sz, err := strconv.ParseInt(strings.TrimSpace(strings.Split(ln, ";")[0]), 16, 64)
			if err != nil || sz < 0 {
				return nil, fmt.Errorf("bad chunk size")
			}
			if sz == 0 {
				// trailer headers until blank line
				for {
					t, err := cn.readLine()
					if err != nil {
						return nil, err
					}
					if t == "" {
						break
					}
				}
				break
			}
			b, err := cn.readN(int(sz))
			if err != nil {
				return nil, err
			}
			rq.body = append(rq.body, b...)
			if _, err := cn.readLine(); err != nil {
				return nil, err
			}
		}
	}
	connHdr := strings.ToLower(rq.headers["connection"])
	if strings.Contains(connHdr, "close") {
		cn.keep = false
	} else {
		cn.keep = true
	}
	return rq, nil
}

func parseQuery(s string, out map[string]string) {
	for _, kv := range strings.Split(s, "&") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		out[unescape(strings.ReplaceAll(k, "+", " "))] = unescape(strings.ReplaceAll(v, "+", " "))
	}
}

func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------- response writing ----------

var statusText = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

var hdrBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 1024); return &b }}

// writeRaw sends one complete HTTP response in a single Write call.
func (cn *conn) writeRaw(status int, contentType string, extra map[string]string, body []byte, req *request) {
	pb := hdrBufPool.Get().(*[]byte)
	buf := (*pb)[:0]
	buf = append(buf, "HTTP/1.1 "...)
	buf = strconv.AppendInt(buf, int64(status), 10)
	buf = append(buf, ' ')
	buf = append(buf, statusText[status]...)
	buf = append(buf, "\r\n"...)
	if contentType != "" {
		buf = append(buf, "Content-Type: "...)
		buf = append(buf, contentType...)
		buf = append(buf, "\r\n"...)
	}
	// request id on every response
	id := reqSeq.Add(1)
	buf = append(buf, "X-GitHub-Request-Id: req-"...)
	buf = strconv.AppendInt(buf, id, 10)
	buf = append(buf, "\r\n"...)
	if extra != nil {
		for k, v := range extra {
			buf = append(buf, k...)
			buf = append(buf, ": "...)
			buf = append(buf, v...)
			buf = append(buf, "\r\n"...)
		}
	}
	if !cn.keep {
		buf = append(buf, "Connection: close\r\n"...)
	}
	buf = append(buf, "Content-Length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, "\r\n\r\n"...)
	if len(body) > 0 {
		buf = append(buf, body...)
	}
	cn.c.Write(buf)
	*pb = buf[:0]
	hdrBufPool.Put(pb)
}

func (cn *conn) writeJSON(status int, body []byte, req *request, extra map[string]string) {
	cn.writeRaw(status, "application/json", extra, body, req)
}

func errJSON(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"message": msg})
	return b
}

func (cn *conn) writeErr(status int, msg string, req *request) {
	cn.writeJSON(status, errJSON(msg), req, nil)
}

// ---------- auth ----------

func authed(rq *request) bool {
	if token == "" {
		return true
	}
	h := rq.headers["authorization"]
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return h[7:] == token
}

func sha1Hex(b []byte) string {
	s := sha1.Sum(b)
	return fmt.Sprintf("%x", s)
}

// ---------- dispatch ----------

func handle(cn *conn, rq *request) {
	defer func() {
		if r := recover(); r != nil {
			cn.writeErr(500, "internal error", rq)
		}
	}()
	p := rq.path
	if p == "/healthz" {
		if rq.method == "GET" {
			cn.writeJSON(200, []byte(`{"ok":true}`), rq, nil)
			return
		}
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if strings.HasPrefix(p, "/api/v3") {
		handleREST(cn, rq)
		return
	}
	if p == "/api/graphql" {
		handleGraphQL(cn, rq)
		return
	}
	if p == "/mcp" {
		handleMCP(cn, rq)
		return
	}
	if strings.HasSuffix(p, ".git/info/refs") || strings.HasSuffix(p, ".git/git-upload-pack") || strings.HasSuffix(p, ".git/git-receive-pack") {
		handleGit(cn, rq)
		return
	}
	cn.writeErr(404, "Not Found", rq)
}

func serveConn(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	cn := &conn{c: c, rb: make([]byte, 0, 65536), keep: true}
	for {
		rq, err := cn.readRequest()
		if err != nil || rq == nil {
			return
		}
		handle(cn, rq)
		if !cn.keep {
			return
		}
	}
}

func main() {
	token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Printf("ready %d\n", ln.Addr().(*net.TCPAddr).Port)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go serveConn(c)
	}
}
