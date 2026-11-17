// In-memory store: repositories, git objects, issues, pulls.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

var typeNames = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

type Obj struct {
	typ  int
	data []byte
}

type Comment struct {
	id   int
	body string
}

type Issue struct {
	repo      *Repo
	number    int
	id        int
	title     string
	body      string
	state     string // "open" | "closed"
	labels    []string
	comments  []*Comment
	seq       int
	jsonCache []byte
}

type FileChange struct {
	filename  string
	status    string // "added" | "modified" | "removed"
	additions int
	deletions int
}

type Pull struct {
	repo         *Repo
	number       int
	id           int
	title        string
	body         string
	headRef      string
	baseRef      string
	headSha      string
	baseSha      string
	baseAtCreate string
	state        string // "open" | "closed"
	merged       bool
	mergeSha     string
	mergedAt     string
	closedAt     string
	seq          int
	files        []FileChange
	comments     []*Comment
}

type packEntry struct {
	gen       int
	pack      []byte
	shallow   []string
	unshallow []string
}

type Repo struct {
	id            int
	name          string
	owner         atomic.Value // string
	description   string
	private       bool
	defaultBranch string
	refs          map[string]string
	objects       map[string]*Obj
	labels        map[string]string
	issues        []*Issue
	issuesByNum   map[int]*Issue
	pulls         []*Pull
	pullsByNum    map[int]*Pull
	gen           int
	seq           int
	jsonCache     []byte
	cacheMu       sync.Mutex
	advCache      map[string][]byte
	packCache     map[string]*packEntry
}

type Store struct {
	mu         sync.RWMutex
	bindMu     sync.Mutex
	repos      map[string]*Repo
	repoByID   map[int]*Repo
	nextRepoID int
	nextID     int // global sequential id for issues/pulls/comments
	viewer     atomic.Value // string
	seq        int
}

func (r *Repo) getOwner() string {
	if v := r.owner.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (r *Repo) setOwner(s string) { r.owner.Store(s) }

func (s *Store) getViewer() string {
	if v := s.viewer.Load(); v != nil {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return ""
}

func NewStore() *Store {
	return &Store{repos: map[string]*Repo{}, repoByID: map[int]*Repo{}, nextRepoID: 1, nextID: 1}
}

func (s *Store) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *Store) learnOwner(owner string) {
	if owner == "" {
		return
	}
	s.bindMu.Lock()
	if s.getViewer() == "" {
		s.viewer.Store(owner)
	}
	s.bindMu.Unlock()
}

// ownerLogin returns the effective owner login for a repo.
func (r *Repo) ownerLogin() string {
	if o := r.getOwner(); o != "" {
		return o
	}
	if v := store.getViewer(); v != "" {
		return v
	}
	return "viewer"
}

// repoByName returns the repo with this name, binding it to owner on first
// path-addressed access. Caller must hold at least a read lock.
func (s *Store) repoByName(owner, name string) *Repo {
	r := s.repos[name]
	if r == nil {
		return nil
	}
	if r.getOwner() == "" {
		s.bindMu.Lock()
		if r.getOwner() == "" {
			r.setOwner(owner)
			if s.getViewer() == "" {
				s.viewer.Store(owner)
			}
		}
		s.bindMu.Unlock()
	}
	if r.getOwner() != owner {
		return nil
	}
	return r
}

func (s *Store) createRepo(name, description string, priv bool) *Repo {
	s.nextRepoID++
	r := &Repo{
		id:            s.nextRepoID - 1,
		name:          name,
		description:   description,
		private:       priv,
		defaultBranch: "main",
		refs:          map[string]string{},
		objects:       map[string]*Obj{},
		labels:        map[string]string{},
		issuesByNum:   map[int]*Issue{},
		pullsByNum:    map[int]*Pull{},
		advCache:      map[string][]byte{},
		packCache:     map[string]*packEntry{},
		seq:           s.nextSeq(),
	}
	s.repos[name] = r
	s.repoByID[r.id] = r
	if v := s.getViewer(); v != "" {
		r.setOwner(v)
	}
	return r
}

func (s *Store) deleteRepo(r *Repo) {
	delete(s.repos, r.name)
	delete(s.repoByID, r.id)
}

// ---------- git objects ----------

func storeObjIDOnly(typ int, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typeNames[typ], len(data))))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func storeObj(r *Repo, typ int, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typeNames[typ], len(data))))
	h.Write(data)
	oid := fmt.Sprintf("%x", h.Sum(nil))
	if _, ok := r.objects[oid]; !ok {
		d := make([]byte, len(data))
		copy(d, data)
		r.objects[oid] = &Obj{typ: typ, data: d}
	}
	return oid
}

type commitInfo struct {
	tree    string
	parents []string
	message string
	raw     []byte
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	c.raw = data
	// header lines until blank line
	i := bytes.Index(data, []byte("\n\n"))
	head := data
	if i >= 0 {
		head = data[:i]
		c.message = string(data[i+2:])
	}
	for _, ln := range bytes.Split(head, []byte("\n")) {
		if bytes.HasPrefix(ln, []byte("tree ")) {
			c.tree = string(ln[5:])
		} else if bytes.HasPrefix(ln, []byte("parent ")) {
			c.parents = append(c.parents, string(ln[7:]))
		}
	}
	return c
}

type treeEntry struct {
	mode string
	name string
	oid  string
	dir  bool
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(data[i:], 0)
		if nul < 0 || i+nul+20 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		oid := fmt.Sprintf("%x", data[i+nul+1:i+nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid, dir: mode == "40000"})
		i += nul + 21
	}
	return out
}

// flatTree maps path -> blob oid for the tree at commitOid.
func flatTree(r *Repo, commitOid string) map[string]string {
	out := map[string]string{}
	o := r.objects[commitOid]
	if o == nil || o.typ != objCommit {
		return out
	}
	c := parseCommit(o.data)
	var walk func(treeOid, prefix string)
	walk = func(treeOid, prefix string) {
		to := r.objects[treeOid]
		if to == nil {
			return
		}
		for _, e := range parseTree(to.data) {
			if e.dir {
				walk(e.oid, prefix+e.name+"/")
			} else {
				out[prefix+e.name] = e.oid
			}
		}
	}
	walk(c.tree, "")
	return out
}

func sortedEntries(flat map[string]string) []string {
	keys := make([]string, 0, len(flat))
	for k := range flat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildTree builds (and stores) the tree for a flat path->blob map.
func buildTree(r *Repo, flat map[string]string) string {
	var build func(paths []string, prefix string) string
	build = func(paths []string, prefix string) string {
		type group struct {
			name  string
			dir   bool
			paths []string
		}
		var groups []group
		byName := map[string]int{}
		for _, p := range paths {
			rest := strings.TrimPrefix(p, prefix)
			seg, tail, isDir := rest, "", false
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				seg, tail, isDir = rest[:j], rest[j+1:], true
			}
			_ = tail
			if idx, ok := byName[seg]; ok {
				groups[idx].paths = append(groups[idx].paths, p)
			} else {
				byName[seg] = len(groups)
				groups = append(groups, group{name: seg, dir: isDir, paths: []string{p}})
			}
		}
		sort.Slice(groups, func(a, b int) bool {
			ka, kb := groups[a].name, groups[b].name
			if groups[a].dir {
				ka += "/"
			}
			if groups[b].dir {
				kb += "/"
			}
			return ka < kb
		})
		var buf bytes.Buffer
		for _, g := range groups {
			if g.dir {
				sub := build(g.paths, prefix+g.name+"/")
				buf.WriteString("40000 " + g.name + "\x00")
				var raw [20]byte
				for i := 0; i < 20; i++ {
					var v int
					fmt.Sscanf(sub[i*2:i*2+2], "%02x", &v)
					raw[i] = byte(v)
				}
				buf.Write(raw[:])
			} else {
				buf.WriteString("100644 " + g.name + "\x00")
				oid := flat[prefix+g.name]
				var raw [20]byte
				for i := 0; i < 20; i++ {
					var v int
					fmt.Sscanf(oid[i*2:i*2+2], "%02x", &v)
					raw[i] = byte(v)
				}
				buf.Write(raw[:])
			}
		}
		return storeObj(r, objTree, buf.Bytes())
	}
	keys := sortedEntries(flat)
	return build(keys, "")
}

func makeCommit(r *Repo, tree string, parents []string, message string) string {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author Forge <forge@example.invalid> 1700000000 +0000\n")
	buf.WriteString("committer Forge <forge@example.invalid> 1700000000 +0000\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return storeObj(r, objCommit, buf.Bytes())
}

// reachable returns all object ids reachable from tips (commits, tags, trees, blobs).
func reachable(r *Repo, tips []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		o := r.objects[oid]
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.typ {
		case objCommit:
			c := parseCommit(o.data)
			stack = append(stack, c.tree)
			stack = append(stack, c.parents...)
		case objTag:
			// tag object: find "object <oid>" line
			for _, ln := range bytes.Split(o.data, []byte("\n")) {
				if bytes.HasPrefix(ln, []byte("object ")) {
					stack = append(stack, string(ln[7:]))
				}
			}
		case objTree:
			for _, e := range parseTree(o.data) {
				stack = append(stack, e.oid)
			}
		}
	}
	return seen
}

// shallowClosure returns the commit + its tree closure (no parents).
func shallowClosure(r *Repo, oid string) map[string]bool {
	out := map[string]bool{}
	o := r.objects[oid]
	if o == nil {
		return out
	}
	out[oid] = true
	if o.typ == objCommit {
		c := parseCommit(o.data)
		for t := range reachable(r, []string{c.tree}) {
			out[t] = true
		}
	} else if o.typ == objTag {
		for t := range reachable(r, []string{oid}) {
			out[t] = true
		}
	}
	return out
}

func isAncestor(r *Repo, anc, tip string) bool {
	if anc == tip {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := r.objects[oid]
		if o == nil || o.typ != objCommit {
			continue
		}
		c := parseCommit(o.data)
		for _, p := range c.parents {
			if p == anc {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// diffFlat returns file changes from tree a to tree b.
func diffFlat(a, b map[string]string) []FileChange {
	var out []FileChange
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	ks := make([]string, 0, len(keys))
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		ao, aok := a[k]
		bo, bok := b[k]
		switch {
		case aok && !bok:
			out = append(out, FileChange{filename: k, status: "removed", deletions: 1})
		case !aok && bok:
			out = append(out, FileChange{filename: k, status: "added", additions: 1})
		case ao != bo:
			out = append(out, FileChange{filename: k, status: "modified", additions: 1, deletions: 1})
		}
	}
	return out
}

// ---------- pack writing ----------

func hexToBytes(s string) []byte {
	out := make([]byte, 20)
	for i := 0; i < 20; i++ {
		var v int
		fmt.Sscanf(s[i*2:i*2+2], "%02x", &v)
		out[i] = byte(v)
	}
	return out
}

func writePackObject(buf *bytes.Buffer, typ int, data []byte) {
	// varint header: type (3 bits) + size
	size := len(data)
	first := byte((typ << 4) | (size & 0x0f))
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	buf.WriteByte(first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	var zb bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&zb, zlib.BestSpeed)
	zw.Write(data)
	zw.Close()
	buf.Write(zb.Bytes())
}

// buildPack serializes the given object ids as a full-object pack.
func buildPack(r *Repo, oids map[string]bool) []byte {
	list := make([]string, 0, len(oids))
	for oid := range oids {
		list = append(list, oid)
	}
	sort.Strings(list)
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(list)))
	buf.Write(hdr[:])
	for _, oid := range list {
		o := r.objects[oid]
		if o == nil {
			continue
		}
		writePackObject(&buf, o.typ, o.data)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// selectFetch computes the object set for a fetch request.
func selectFetch(r *Repo, wants, haves, shallow []string, depth int) (map[string]bool, []string, []string) {
	shallowSet := map[string]bool{}
	for _, s := range shallow {
		shallowSet[s] = true
	}
	// exclusion: objects the client already holds
	excl := map[string]bool{}
	for _, h := range haves {
		if shallowSet[h] {
			for o := range shallowClosure(r, h) {
				excl[o] = true
			}
		} else {
			for o := range reachable(r, []string{h}) {
				excl[o] = true
			}
		}
	}
	included := map[string]bool{}
	var newShallow, newUnshallow []string
	if depth > 0 {
		// BFS generations from wants
		type item struct {
			oid string
			gen int
		}
		queue := []item{}
		seenCommit := map[string]bool{}
		for _, w := range wants {
			// peel tags
			o := r.objects[w]
			if o == nil {
				continue
			}
			if o.typ == objTag {
				included[w] = true
				for _, ln := range bytes.Split(o.data, []byte("\n")) {
					if bytes.HasPrefix(ln, []byte("object ")) {
						queue = append(queue, item{string(ln[7:]), 0})
					}
				}
			} else {
				queue = append(queue, item{w, 0})
			}
		}
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			if seenCommit[it.oid] || it.gen >= depth {
				continue
			}
			seenCommit[it.oid] = true
			o := r.objects[it.oid]
			if o == nil || o.typ != objCommit {
				continue
			}
			included[it.oid] = true
			c := parseCommit(o.data)
			for t := range reachable(r, []string{c.tree}) {
				included[t] = true
			}
			if it.gen+1 < depth {
				for _, p := range c.parents {
					queue = append(queue, item{p, it.gen + 1})
				}
				if shallowSet[it.oid] {
					newUnshallow = append(newUnshallow, it.oid)
				}
			} else if len(c.parents) > 0 {
				newShallow = append(newShallow, it.oid)
			}
		}
	} else {
		for o := range reachable(r, wants) {
			included[o] = true
		}
	}
	for o := range excl {
		delete(included, o)
	}
	sort.Strings(newShallow)
	sort.Strings(newUnshallow)
	return included, newShallow, newUnshallow
}

// ---------- JSON helpers ----------

func b64id(kind string, parts ...int) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteByte(':')
		sb.WriteString(fmt.Sprint(p))
	}
	return base64.StdEncoding.EncodeToString([]byte(sb.String()))
}

func repoJSON(r *Repo) []byte {
	if r.jsonCache != nil {
		return r.jsonCache
	}
	owner := r.ownerLogin()
	m := map[string]any{
		"id":             r.id,
		"node_id":        b64id("repo", r.id),
		"name":           r.name,
		"full_name":      owner + "/" + r.name,
		"owner":          map[string]any{"login": owner, "id": r.id, "type": "User", "node_id": b64id("user", r.id)},
		"private":        r.private,
		"description":    r.description,
		"default_branch": r.defaultBranch,
		"html_url":       "https://example.invalid/" + owner + "/" + r.name,
		"url":            "/api/v3/repos/" + owner + "/" + r.name,
		"clone_url":      "https://example.invalid/" + owner + "/" + r.name + ".git",
		"git_url":        "git://example.invalid/" + owner + "/" + r.name + ".git",
		"ssh_url":        "git@example.invalid:" + owner + "/" + r.name + ".git",
		"created_at":     fixedTS,
		"updated_at":     fixedTS,
		"pushed_at":      fixedTS,
		"has_issues":     true,
		"has_projects":   true,
		"has_downloads":  true,
		"has_wiki":       true,
		"has_pages":      false,
		"forks_count":    0,
		"stargazers_count": 0,
		"watchers_count":   0,
		"open_issues_count": len(r.issues),
		"size":           0,
		"archived":       false,
		"disabled":       false,
		"fork":           false,
		"visibility":     "public",
	}
	b, _ := json.Marshal(m)
	r.jsonCache = b
	return b
}

func issueLabelsJSON(iss *Issue) []any {
	out := make([]any, 0, len(iss.labels))
	for _, l := range iss.labels {
		color := iss.repo.labels[l]
		out = append(out, map[string]any{"name": l, "color": color})
	}
	return out
}

func buildIssueJSON(iss *Issue) []byte {
	owner := iss.repo.ownerLogin()
	var b strings.Builder
	b.Grow(560)
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(iss.id))
	b.WriteString(`,"node_id":`)
	b.WriteString(strconv.Quote(b64id("issue", iss.repo.id, iss.number)))
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(iss.number))
	b.WriteString(`,"title":`)
	b.WriteString(strconv.Quote(iss.title))
	b.WriteString(`,"body":`)
	b.WriteString(strconv.Quote(iss.body))
	b.WriteString(`,"state":`)
	b.WriteString(strconv.Quote(iss.state))
	b.WriteString(`,"labels":[`)
	for i, l := range iss.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		b.WriteString(strconv.Quote(l))
		b.WriteString(`,"color":`)
		b.WriteString(strconv.Quote(iss.repo.labels[l]))
		b.WriteByte('}')
	}
	b.WriteString(`],"user":{"login":`)
	b.WriteString(strconv.Quote(owner))
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(iss.repo.id))
	b.WriteString(`,"type":"User"},"assignees":[],"comments":`)
	b.WriteString(strconv.Itoa(len(iss.comments)))
	b.WriteString(`,"created_at":"` + fixedTS + `","updated_at":"` + fixedTS + `","closed_at":`)
	if iss.state == "closed" {
		b.WriteString(`"` + fixedTS + `"`)
	} else {
		b.WriteString("null")
	}
	b.WriteString(`,"html_url":"https://example.invalid/` + owner + `/` + iss.repo.name + `/issues/` + strconv.Itoa(iss.number) + `"`)
	b.WriteString(`,"url":"/api/v3/repos/` + owner + `/` + iss.repo.name + `/issues/` + strconv.Itoa(iss.number) + `"`)
	b.WriteString(`,"repository_url":"/api/v3/repos/` + owner + `/` + iss.repo.name + `"}`)
	return []byte(b.String())
}

func issueJSON(iss *Issue) []byte {
	if iss.jsonCache == nil {
		iss.jsonCache = buildIssueJSON(iss)
	}
	return iss.jsonCache
}

func (r *Repo) invalidate() {
	r.gen++
	r.jsonCache = nil
	r.cacheMu.Lock()
	r.advCache = map[string][]byte{}
	r.packCache = map[string]*packEntry{}
	r.cacheMu.Unlock()
}
