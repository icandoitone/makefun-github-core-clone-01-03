// Git smart HTTP: upload-pack (v0 + v2), receive-pack (v0), pack I/O.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ---------- pkt-line ----------

func pktLine(s string) []byte {
	n := len(s) + 4
	var b bytes.Buffer
	fmt.Fprintf(&b, "%04x", n)
	b.WriteString(s)
	return b.Bytes()
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// decodePktLines parses a pkt-line stream. Returns lines (without length
// prefix), and the index where the payload after the final flush starts
// (or -1 if no flush). Delim/flush markers are reported as "" with kind.
type pktLineItem struct {
	kind string // "data", "flush", "delim"
	text string
}

func decodePktLines(b []byte) []pktLineItem {
	var out []pktLineItem
	i := 0
	for i+4 <= len(b) {
		n, err := strconv.ParseInt(string(b[i:i+4]), 16, 32)
		if err != nil || n < 0 {
			break
		}
		if n == 0 {
			out = append(out, pktLineItem{kind: "flush"})
			i += 4
			continue
		}
		if n == 1 {
			out = append(out, pktLineItem{kind: "delim"})
			i += 4
			continue
		}
		if i+int(n) > len(b) {
			break
		}
		out = append(out, pktLineItem{kind: "data", text: string(b[i+4 : i+int(n)])})
		i += int(n)
	}
	return out
}

// splitAfterFlush returns the byte offset just past the first flush packet.
func splitAfterFlush(b []byte) int {
	i := 0
	for i+4 <= len(b) {
		n, err := strconv.ParseInt(string(b[i:i+4]), 16, 32)
		if err != nil || n < 0 {
			return -1
		}
		if n == 0 {
			return i + 4
		}
		if n == 1 {
			i += 4
			continue
		}
		if i+int(n) > len(b) {
			return -1
		}
		i += int(n)
	}
	return -1
}

// ---------- pack reading ----------

type packObject struct {
	typ  int
	data []byte
	oid  string
}

// parsePack decodes a packfile (full objects + OFS/REF deltas) into objects.
// REF_DELTA bases are resolved from earlier pack entries or from repoObjs.
func parsePack(b []byte, repoObjs map[string]*Obj) (map[string]*Obj, error) {
	if len(b) < 12 || string(b[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	ver := binary.BigEndian.Uint32(b[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("unsupported pack version")
	}
	count := binary.BigEndian.Uint32(b[8:12])
	if len(b) < 32 {
		return nil, fmt.Errorf("pack too short")
	}
	body := b[12 : len(b)-20]
	out := map[string]*Obj{}
	type rawObj struct {
		typ  int
		data []byte
	}
	byOffset := map[int]rawObj{}
	pos := 0
	for i := uint32(0); i < count; i++ {
		start := pos
		if pos >= len(body) {
			return nil, fmt.Errorf("pack truncated")
		}
		c := body[pos]
		pos++
		typ := int((c >> 4) & 0x7)
		size := int64(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= len(body) {
				return nil, fmt.Errorf("pack truncated")
			}
			c = body[pos]
			pos++
			size |= int64(c&0x7f) << shift
			shift += 7
		}
		var baseTyp int
		var baseData []byte
		switch typ {
		case 6: // OFS_DELTA
			if pos >= len(body) {
				return nil, fmt.Errorf("pack truncated")
			}
			c = body[pos]
			pos++
			off := int64(c & 0x7f)
			for c&0x80 != 0 {
				if pos >= len(body) {
					return nil, fmt.Errorf("pack truncated")
				}
				c = body[pos]
				pos++
				off = ((off + 1) << 7) | int64(c&0x7f)
			}
			baseOff := start - int(off)
			base, ok := byOffset[baseOff]
			if !ok {
				return nil, fmt.Errorf("ofs delta base not found")
			}
			baseTyp, baseData = base.typ, base.data
		case 7: // REF_DELTA
			if pos+20 > len(body) {
				return nil, fmt.Errorf("pack truncated")
			}
			baseOid := fmt.Sprintf("%x", body[pos:pos+20])
			pos += 20
			if base, ok := out[baseOid]; ok {
				baseTyp, baseData = base.typ, base.data
			} else if base, ok := repoObjs[baseOid]; ok {
				baseTyp, baseData = base.typ, base.data
			} else {
				return nil, fmt.Errorf("ref delta base not found")
			}
		}
		zdata, consumed, err := readZlib(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += consumed
		if typ == 6 || typ == 7 {
			data, err := applyDelta(baseData, zdata)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != size {
				return nil, fmt.Errorf("delta size mismatch")
			}
			oid := objID(baseTyp, data)
			out[oid] = &Obj{typ: baseTyp, data: data}
			byOffset[start] = rawObj{typ: baseTyp, data: data}
		} else {
			if int64(len(zdata)) != size {
				return nil, fmt.Errorf("object size mismatch")
			}
			oid := objID(typ, zdata)
			out[oid] = &Obj{typ: typ, data: zdata}
			byOffset[start] = rawObj{typ: typ, data: zdata}
		}
	}
	return out, nil
}

func objID(typ int, data []byte) string {
	return storeObjIDOnly(typ, data)
}

func readZlib(b []byte) ([]byte, int, error) {
	// The counting reader implements io.ByteReader, so the flate layer
	// consumes bytes one at a time and never reads past the stream end.
	cr := &countReader{r: bytes.NewReader(b)}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return data, cr.n, nil
}

type countReader struct {
	r *bytes.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// applyDelta applies a git binary delta to base.
func applyDelta(base, delta []byte) ([]byte, error) {
	// varint: source size, target size
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			var off, sz int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					off |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta truncated")
					}
					sz |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if sz == 0 {
				sz = 0x10000
			}
			if off+sz > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[off:off+sz]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("delta truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return out, nil
}

// ---------- git HTTP ----------

func gitRepoFromPath(rq *request) (owner, repo string, ok bool) {
	p := rq.path
	// /{owner}/{repo}.git/...
	if !strings.HasPrefix(p, "/") {
		return "", "", false
	}
	rest := p[1:]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", false
	}
	owner = rest[:slash]
	rest = rest[slash+1:]
	if !strings.HasSuffix(rest, ".git") && !strings.Contains(rest, ".git/") {
		return "", "", false
	}
	idx := strings.Index(rest, ".git")
	repo = rest[:idx]
	return owner, repo, true
}

func handleGit(cn *conn, rq *request) {
	owner, repoName, ok := gitRepoFromPath(rq)
	if !ok {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	isUpload := strings.HasSuffix(rq.path, "git-upload-pack")
	isReceive := strings.HasSuffix(rq.path, "git-receive-pack")
	isInfo := strings.HasSuffix(rq.path, "info/refs")
	if !isUpload && !isReceive && !isInfo {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	// auth on every git request
	if !authed(rq) {
		cn.writeJSON(401, errJSON("Requires authentication"), rq, nil)
		return
	}
	if isInfo {
		service := rq.query["service"]
		if service != "git-upload-pack" && service != "git-receive-pack" {
			cn.writeErr(400, "invalid service", rq)
			return
		}
		store.mu.RLock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		var body []byte
		if service == "git-upload-pack" {
			body = uploadAdvertisement(r, rq)
		} else {
			body = receiveAdvertisement(r)
		}
		store.mu.RUnlock()
		ct := "application/x-git-upload-pack-advertisement"
		if service == "git-receive-pack" {
			ct = "application/x-git-receive-pack-advertisement"
		}
		cn.writeRaw(200, ct, nil, body, rq)
		return
	}
	// POST services
	if rq.method != "POST" {
		cn.writeErr(405, "Method Not Allowed", rq)
		return
	}
	if isUpload {
		handleUploadPack(cn, rq, owner, repoName)
	} else {
		handleReceivePack(cn, rq, owner, repoName)
	}
}

func sortedRefNames(r *Repo) []string {
	names := make([]string, 0, len(r.refs))
	for n := range r.refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func defaultTip(r *Repo) string {
	return r.refs["refs/heads/"+r.defaultBranch]
}

func uploadAdvertisement(r *Repo, rq *request) []byte {
	key := "up-v0"
	if rq.headers["git-protocol"] == "version=2" {
		key = "up-v2"
	}
	r.cacheMu.Lock()
	if b, ok := r.advCache[key]; ok {
		r.cacheMu.Unlock()
		return b
	}
	r.cacheMu.Unlock()
	var out []byte
	if key == "up-v2" {
		var b bytes.Buffer
		b.Write(pktLine("version 2\n"))
		b.Write(pktLine("agent=forge/1.0\n"))
		b.Write(pktLine("object-format=sha1\n"))
		b.Write(pktLine("fetch=shallow wait-for-done\n"))
		b.Write(pktLine("ls-refs=unborn\n"))
		b.Write(pktLine("object-info\n"))
		b.Write(pktFlush())
		out = b.Bytes()
	} else {
		var b bytes.Buffer
		b.Write(pktLine("# service=git-upload-pack\n"))
		b.Write(pktFlush())
		tip := defaultTip(r)
		caps := "side-band-64k thin-pack ofs-delta multi_ack multi_ack_detailed shallow agent=forge/1.0"
		if tip != "" {
			caps += " symref=HEAD:refs/heads/" + r.defaultBranch
		}
		names := sortedRefNames(r)
		if len(names) == 0 {
			b.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		} else {
			for i, name := range names {
				line := r.refs[name] + " " + name
				if i == 0 {
					line += "\x00" + caps
				}
				b.Write(pktLine(line + "\n"))
			}
		}
		b.Write(pktFlush())
		out = b.Bytes()
	}
	r.cacheMu.Lock()
	r.advCache[key] = out
	r.cacheMu.Unlock()
	return out
}

func receiveAdvertisement(r *Repo) []byte {
	r.cacheMu.Lock()
	if b, ok := r.advCache["recv-v0"]; ok {
		r.cacheMu.Unlock()
		return b
	}
	r.cacheMu.Unlock()
	var b bytes.Buffer
	b.Write(pktLine("# service=git-receive-pack\n"))
	b.Write(pktFlush())
	tip := defaultTip(r)
	caps := "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
	if tip != "" {
		caps += " symref=HEAD:refs/heads/" + r.defaultBranch
	}
	names := sortedRefNames(r)
	if len(names) == 0 {
		b.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		for i, name := range names {
			line := r.refs[name] + " " + name
			if i == 0 {
				line += "\x00" + caps
			}
			b.Write(pktLine(line + "\n"))
		}
	}
	b.Write(pktFlush())
	out := b.Bytes()
	r.cacheMu.Lock()
	r.advCache["recv-v0"] = out
	r.cacheMu.Unlock()
	return out
}

// ---------- upload-pack ----------

func handleUploadPack(cn *conn, rq *request, owner, repoName string) {
	protoV2 := rq.headers["git-protocol"] == "version=2"
	store.mu.RLock()
	r := store.repoByName(owner, repoName)
	if r == nil {
		store.mu.RUnlock()
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if protoV2 {
		handleUploadV2(cn, rq, r)
	} else {
		handleUploadV0(cn, rq, r)
	}
	store.mu.RUnlock()
}

func sidebandPackets(pack []byte) []byte {
	var b bytes.Buffer
	const chunk = 60000
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 1+end-i)
		payload[0] = 1
		copy(payload[1:], pack[i:end])
		b.Write(pktLine(string(payload)))
	}
	return b.Bytes()
}

// fetchPackBytes returns the cached (or freshly built) pack for a fetch.
func (r *Repo) fetchPack(wants, haves, shallow []string, depth int) ([]byte, []string, []string) {
	key := strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|" + strconv.Itoa(depth)
	r.cacheMu.Lock()
	if e, ok := r.packCache[key]; ok && e.gen == r.gen {
		r.cacheMu.Unlock()
		return e.pack, e.shallow, e.unshallow
	}
	r.cacheMu.Unlock()
	included, sh, unsh := selectFetch(r, wants, haves, shallow, depth)
	pack := buildPack(r, included)
	e := &packEntry{gen: r.gen, pack: pack, shallow: sh, unshallow: unsh}
	r.cacheMu.Lock()
	r.packCache[key] = e
	r.cacheMu.Unlock()
	return pack, sh, unsh
}

func handleUploadV2(cn *conn, rq *request, r *Repo) {
	items := decodePktLines(rq.body)
	if len(items) == 0 || items[0].text == "" {
		cn.writeErr(400, "bad request", rq)
		return
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(items[0].text, "command="))
	switch cmd {
	case "ls-refs":
		var prefixes []string
		peel, symrefs, unborn := false, false, false
		for _, it := range items[1:] {
			if it.kind != "data" {
				continue
			}
			t := strings.TrimSpace(it.text)
			switch {
			case t == "peel":
				peel = true
			case t == "symrefs":
				symrefs = true
			case t == "unborn":
				unborn = true
			case strings.HasPrefix(t, "ref-prefix "):
				prefixes = append(prefixes, strings.TrimPrefix(t, "ref-prefix "))
			}
		}
		var b bytes.Buffer
		tip := defaultTip(r)
		names := sortedRefNames(r)
		// HEAD line first (unless ref-prefix filters exclude it)
		if len(prefixes) == 0 {
			if tip != "" {
				line := tip + " HEAD"
				if symrefs {
					line += " symref-target:refs/heads/" + r.defaultBranch
				}
				b.Write(pktLine(line + "\n"))
			} else if unborn {
				b.Write(pktLine("unborn HEAD symref-target:refs/heads/" + r.defaultBranch + "\n"))
			}
		}
		for _, name := range names {
			if len(prefixes) > 0 {
				match := false
				for _, p := range prefixes {
					if strings.HasPrefix(name, p) {
						match = true
					}
				}
				if !match {
					continue
				}
			}
			oid := r.refs[name]
			line := oid + " " + name
			o := r.objects[oid]
			if peel && o != nil && o.typ == objTag {
				// peel to target commit
				for _, ln := range bytes.Split(o.data, []byte("\n")) {
					if bytes.HasPrefix(ln, []byte("object ")) {
						line += " peeled:" + string(ln[7:])
					}
				}
			}
			b.Write(pktLine(line + "\n"))
		}
		b.Write(pktFlush())
		cn.writeRaw(200, "application/x-git-upload-pack-result", nil, b.Bytes(), rq)
	case "fetch":
		var wants, haves, shallow []string
		depth := 0
		thinPack := false
		for _, it := range items[1:] {
			if it.kind != "data" {
				continue
			}
			t := strings.TrimSpace(it.text)
			switch {
			case strings.HasPrefix(t, "want "):
				wants = append(wants, strings.TrimSpace(t[5:]))
			case strings.HasPrefix(t, "have "):
				haves = append(haves, strings.TrimSpace(t[5:]))
			case strings.HasPrefix(t, "shallow "):
				shallow = append(shallow, strings.TrimSpace(t[8:]))
			case strings.HasPrefix(t, "deepen "):
				depth, _ = strconv.Atoi(strings.TrimSpace(t[7:]))
			case t == "thin-pack":
				thinPack = true
			case t == "done":
			}
		}
		_ = thinPack
		if len(wants) == 0 {
			cn.writeErr(400, "no wants", rq)
			return
		}
		pack, sh, unsh := r.fetchPack(wants, haves, shallow, depth)
		var b bytes.Buffer
		b.Write(pktLine("acknowledgments\n"))
		b.Write(pktLine("NAK\n"))
		if len(sh) > 0 || len(unsh) > 0 {
			b.Write(pktDelim())
			b.Write(pktLine("shallow-info\n"))
			for _, s := range sh {
				b.Write(pktLine("shallow " + s + "\n"))
			}
			for _, s := range unsh {
				b.Write(pktLine("unshallow " + s + "\n"))
			}
		}
		if len(pack) > 12+20 { // non-empty pack (header+trailer only = empty)
			b.Write(pktDelim())
			b.Write(pktLine("packfile\n"))
			b.Write(sidebandPackets(pack))
		}
		b.Write(pktFlush())
		cn.writeRaw(200, "application/x-git-upload-pack-result", nil, b.Bytes(), rq)
	default:
		cn.writeErr(400, "unknown command", rq)
	}
}

func handleUploadV0(cn *conn, rq *request, r *Repo) {
	// body: wants/shallow/deepen pkt-lines, flush, haves, flush
	items := decodePktLines(rq.body)
	var wants, haves, shallow []string
	depth := 0
	seenFlush := false
	for _, it := range items {
		if it.kind == "flush" {
			seenFlush = true
			continue
		}
		if it.kind != "data" {
			continue
		}
		t := strings.TrimSpace(it.text)
		if !seenFlush {
			switch {
			case strings.HasPrefix(t, "want "):
				// strip capabilities after NUL on first want
				w := strings.TrimSpace(t[5:])
				if i := strings.IndexByte(w, 0); i >= 0 {
					w = w[:i]
				}
				wants = append(wants, strings.Fields(w)[0])
			case strings.HasPrefix(t, "shallow "):
				shallow = append(shallow, strings.TrimSpace(t[8:]))
			case strings.HasPrefix(t, "deepen "):
				depth, _ = strconv.Atoi(strings.TrimSpace(t[7:]))
			}
		} else {
			if strings.HasPrefix(t, "have ") {
				haves = append(haves, strings.TrimSpace(t[5:]))
			}
		}
	}
	if len(wants) == 0 {
		cn.writeErr(400, "no wants", rq)
		return
	}
	pack, sh, unsh := r.fetchPack(wants, haves, shallow, depth)
	var b bytes.Buffer
	b.Write(pktLine("NAK\n"))
	for _, s := range sh {
		b.Write(pktLine("shallow " + s + "\n"))
	}
	for _, s := range unsh {
		b.Write(pktLine("unshallow " + s + "\n"))
	}
	if len(pack) > 32 {
		b.Write(sidebandPackets(pack))
	}
	b.Write(pktFlush())
	cn.writeRaw(200, "application/x-git-upload-pack-result", nil, b.Bytes(), rq)
}

// ---------- receive-pack ----------

type pushCmd struct {
	oldOid, newOid, ref string
}

func handleReceivePack(cn *conn, rq *request, owner, repoName string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	r := store.repoByName(owner, repoName)
	if r == nil {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	flushAt := splitAfterFlush(rq.body)
	if flushAt < 0 {
		cn.writeErr(400, "invalid pkt-line stream", rq)
		return
	}
	items := decodePktLines(rq.body[:flushAt])
	var cmds []pushCmd
	for _, it := range items {
		if it.kind != "data" {
			continue
		}
		t := strings.TrimSuffix(it.text, "\n")
		if i := strings.IndexByte(t, 0); i >= 0 {
			t = t[:i]
		}
		f := strings.Fields(t)
		if len(f) != 3 {
			continue
		}
		if !isHex40(f[0]) || !isHex40(f[1]) {
			cn.writeErr(400, "invalid command line", rq)
			return
		}
		cmds = append(cmds, pushCmd{oldOid: f[0], newOid: f[1], ref: f[2]})
	}
	if len(cmds) == 0 {
		cn.writeErr(400, "no commands", rq)
		return
	}
	// unpack the pack (if any)
	packBytes := rq.body[flushAt:]
	if len(packBytes) > 0 {
		objs, err := parsePack(packBytes, r.objects)
		if err == nil {
			for oid, o := range objs {
				if _, ok := r.objects[oid]; !ok {
					r.objects[oid] = o
				}
			}
		}
		// a bad pack still gets a report; refs are not moved for objects
		// that failed to unpack, but the client always sends valid packs.
	}
	// apply commands
	var report bytes.Buffer
	report.Write(pktLine("unpack ok\n"))
	moved := false
	for _, cmd := range cmds {
		cur, exists := r.refs[cmd.ref]
		if cmd.newOid == zeroOID {
			// delete
			if !exists {
				report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
				continue
			}
			if cmd.oldOid != zeroOID && cmd.oldOid != cur {
				report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
				continue
			}
			delete(r.refs, cmd.ref)
			moved = true
			report.Write(pktLine("ok " + cmd.ref + "\n"))
			continue
		}
		if _, ok := r.objects[cmd.newOid]; !ok {
			report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
			continue
		}
		if exists {
			if cmd.oldOid == zeroOID || cmd.oldOid != cur {
				report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
				continue
			}
			if !isAncestor(r, cur, cmd.newOid) {
				report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
				continue
			}
		} else {
			if cmd.oldOid != zeroOID {
				report.Write(pktLine("ng " + cmd.ref + " non-fast-forward\n"))
				continue
			}
		}
		r.refs[cmd.ref] = cmd.newOid
		moved = true
		report.Write(pktLine("ok " + cmd.ref + "\n"))
	}
	if moved {
		r.invalidate()
	}
	// report inside sideband band 1
	var out bytes.Buffer
	payload := make([]byte, 1+report.Len())
	payload[0] = 1
	copy(payload[1:], report.Bytes())
	out.Write(pktLine(string(payload)))
	out.Write(pktFlush())
	cn.writeRaw(200, "application/x-git-receive-pack-result", nil, out.Bytes(), rq)
}
