// REST v3 API under /api/v3.
package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

func handleREST(cn *conn, rq *request) {
	if !authed(rq) {
		cn.writeJSON(401, errJSON("Requires authentication"), rq, nil)
		return
	}
	p := strings.TrimPrefix(rq.path, "/api/v3")
	if p == "" || p == "/" {
		if rq.method != "GET" {
			cn.writeErr(404, "Not Found", rq)
			return
		}
		body := []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}","meta_url":"/api/v3/meta"}`)
		cn.writeJSON(200, body, rq, map[string]string{"X-OAuth-Scopes": "repo, read:org"})
		return
	}
	segs := splitPath(p)
	switch segs[0] {
	case "meta":
		if rq.method == "GET" && len(segs) == 1 {
			cn.writeJSON(200, []byte(`{"installed_version":"1.0.0"}`), rq, nil)
			return
		}
	case "user":
		if len(segs) == 1 {
			if rq.method == "GET" {
				viewer := store.getViewer()
				if viewer == "" {
					viewer = "viewer"
				}
				m := map[string]any{"login": viewer, "id": 1, "node_id": b64id("user", 1), "type": "User", "html_url": "https://example.invalid/" + viewer, "url": "/api/v3/user", "created_at": fixedTS, "updated_at": fixedTS}
				b, _ := json.Marshal(m)
				cn.writeJSON(200, b, rq, nil)
				return
			}
		} else if segs[1] == "repos" && len(segs) == 2 {
			if rq.method == "POST" {
				handleCreateRepo(cn, rq)
				return
			}
			if rq.method == "GET" {
				handleListUserRepos(cn, rq)
				return
			}
		}
	case "repos":
		if len(segs) >= 3 {
			handleRepoRoutes(cn, rq, segs[1], segs[2], segs[3:])
			return
		}
	}
	cn.writeErr(404, "Not Found", rq)
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ---------- JSON body ----------

func parseBody(rq *request) (map[string]any, bool) {
	if len(rq.body) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(rq.body, &m); err != nil {
		return nil, false
	}
	return m, true
}

func bodyStr(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func bodyBool(m map[string]any, k string) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// ---------- pagination ----------

func pageParams(rq *request) (page, perPage int) {
	page = 1
	perPage = 30
	if v := rq.query["page"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}
	if v := rq.query["per_page"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	return page, perPage
}

func pageURL(rq *request, page, perPage int) string {
	return "http://" + rq.host + rq.path + "?page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(perPage)
}

// slicePage returns the page slice and a Link header value ("" if last page).
func slicePage[T any](rq *request, items []T) ([]T, string) {
	page, perPage := pageParams(rq)
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil, ""
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	var link string
	if end < len(items) {
		last := (len(items) + perPage - 1) / perPage
		link = "<" + pageURL(rq, page+1, perPage) + ">; rel=\"next\", <" + pageURL(rq, last, perPage) + ">; rel=\"last\""
	}
	return items[start:end], link
}

// ---------- repos ----------

func handleCreateRepo(cn *conn, rq *request) {
	m, ok := parseBody(rq)
	if !ok {
		cn.writeErr(400, "Problems parsing JSON", rq)
		return
	}
	name := bodyStr(m, "name")
	if name == "" {
		cn.writeErr(422, "name is required", rq)
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.repos[name]; exists {
		cn.writeJSON(422, errJSON("Repository already exists"), rq, nil)
		return
	}
	r := store.createRepo(name, bodyStr(m, "description"), bodyBool(m, "private"))
	cn.writeJSON(201, repoJSON(r), rq, nil)
}

func handleListUserRepos(cn *conn, rq *request) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var all []*Repo
	for _, r := range store.repos {
		all = append(all, r)
	}
	// creation order
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].seq < all[i].seq {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	page, link := slicePage(rq, all)
	var b strings.Builder
	b.WriteString("[")
	for i, r := range page {
		if i > 0 {
			b.WriteString(",")
		}
		b.Write(repoJSON(r))
	}
	b.WriteString("]")
	var extra map[string]string
	if link != "" {
		extra = map[string]string{"Link": link}
	}
	cn.writeJSON(200, []byte(b.String()), rq, extra)
}

func handleRepoRoutes(cn *conn, rq *request, owner, name string, rest []string) {
	// read-only fast path: single issue GET (bench workload)
	if len(rest) == 2 && rest[0] == "issues" && rq.method == "GET" {
		if n, err := strconv.Atoi(rest[1]); err == nil {
			store.mu.RLock()
			r := store.repoByName(owner, name)
			if r == nil {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			iss := r.issuesByNum[n]
			if iss == nil {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			body := issueJSON(iss)
			store.mu.RUnlock()
			cn.writeJSON(200, body, rq, nil)
			return
		}
	}
	if len(rest) == 0 {
		switch rq.method {
		case "GET":
			store.mu.RLock()
			r := store.repoByName(owner, name)
			if r == nil {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			body := repoJSON(r)
			store.mu.RUnlock()
			cn.writeJSON(200, body, rq, nil)
		case "PATCH":
			m, ok := parseBody(rq)
			if !ok {
				cn.writeErr(400, "Problems parsing JSON", rq)
				return
			}
			store.mu.Lock()
			r := store.repoByName(owner, name)
			if r == nil {
				store.mu.Unlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			if d := bodyStr(m, "description"); m["description"] != nil {
				r.description = d
			}
			if v, ok := m["private"]; ok {
				if b, ok := v.(bool); ok {
					r.private = b
				}
			}
			r.invalidate()
			body := repoJSON(r)
			store.mu.Unlock()
			cn.writeJSON(200, body, rq, nil)
		case "DELETE":
			store.mu.Lock()
			r := store.repoByName(owner, name)
			if r == nil {
				store.mu.Unlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			store.deleteRepo(r)
			store.mu.Unlock()
			cn.writeRaw(204, "", nil, nil, rq)
		default:
			cn.writeErr(404, "Not Found", rq)
		}
		return
	}
	switch rest[0] {
	case "branches":
		handleBranches(cn, rq, owner, name, rest[1:])
	case "commits":
		handleCommits(cn, rq, owner, name, rest[1:])
	case "contents":
		handleContents(cn, rq, owner, name, rest[1:])
	case "git":
		handleGitRefs(cn, rq, owner, name, rest[1:])
	case "labels":
		handleLabels(cn, rq, owner, name, rest[1:])
	case "issues":
		handleIssues(cn, rq, owner, name, rest[1:])
	case "pulls":
		handlePulls(cn, rq, owner, name, rest[1:])
	default:
		cn.writeErr(404, "Not Found", rq)
	}
}

func resolveRepo(owner, name string) *Repo {
	return store.repoByName(owner, name)
}

// ---------- branches / commits ----------

func handleBranches(cn *conn, rq *request, owner, name string, rest []string) {
	if rq.method != "GET" {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	r := resolveRepo(owner, name)
	if r == nil {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if len(rest) == 0 {
		names := sortedRefNames(r)
		var branches []map[string]any
		for _, ref := range names {
			if strings.HasPrefix(ref, "refs/heads/") {
				b := strings.TrimPrefix(ref, "refs/heads/")
				branches = append(branches, map[string]any{"name": b, "commit": map[string]any{"sha": r.refs[ref]}, "protected": false})
			}
		}
		if branches == nil {
			branches = []map[string]any{}
		}
		b, _ := json.Marshal(branches)
		cn.writeJSON(200, b, rq, nil)
		return
	}
	branch := strings.Join(rest, "/")
	sha, ok := r.refs["refs/heads/"+branch]
	if !ok {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	m := map[string]any{"name": branch, "commit": map[string]any{"sha": sha}, "protected": false}
	b, _ := json.Marshal(m)
	cn.writeJSON(200, b, rq, nil)
}

func handleCommits(cn *conn, rq *request, owner, name string, rest []string) {
	if rq.method != "GET" || len(rest) != 0 {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	r := resolveRepo(owner, name)
	if r == nil {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	tip := ""
	if q := rq.query["sha"]; q != "" {
		if v, ok := r.refs["refs/heads/"+q]; ok {
			tip = v
		} else if _, ok := r.objects[q]; ok {
			tip = q
		} else {
			cn.writeErr(404, "Not Found", rq)
			return
		}
	} else {
		tip = defaultTip(r)
	}
	var out []map[string]any
	seen := map[string]bool{}
	queue := []string{}
	if tip != "" {
		queue = append(queue, tip)
	}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := r.objects[oid]
		if o == nil || o.typ != objCommit {
			continue
		}
		c := parseCommit(o.data)
		out = append(out, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"message": c.message,
				"author": map[string]any{"name": "Forge", "email": "forge@example.invalid", "date": fixedTS},
				"committer": map[string]any{"name": "Forge", "email": "forge@example.invalid", "date": fixedTS},
				"tree": map[string]any{"sha": c.tree},
			},
			"author": map[string]any{"login": r.ownerLogin()},
			"parents": parentsJSON(c.parents),
		})
		queue = append(queue, c.parents...)
	}
	if out == nil {
		out = []map[string]any{}
	}
	b, _ := json.Marshal(out)
	cn.writeJSON(200, b, rq, nil)
}

func parentsJSON(parents []string) []any {
	out := make([]any, 0, len(parents))
	for _, p := range parents {
		out = append(out, map[string]any{"sha": p})
	}
	return out
}

// ---------- contents ----------

func handleContents(cn *conn, rq *request, owner, name string, rest []string) {
	path := strings.Join(rest, "/")
	switch rq.method {
	case "GET":
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		tip := defaultTip(r)
		if ref := rq.query["ref"]; ref != "" {
			if v, ok := r.refs["refs/heads/"+ref]; ok {
				tip = v
			} else if _, ok := r.objects[ref]; ok {
				tip = ref
			} else {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
		}
		if tip == "" {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		flat := flatTree(r, tip)
		var body []byte
		if path == "" {
			body = dirListing(r, flat, "")
		} else if blobOid, ok := flat[path]; ok {
			o := r.objects[blobOid]
			data := []byte{}
			if o != nil {
				data = o.data
			}
			m := map[string]any{
				"type": "file", "encoding": "base64",
				"content": base64.StdEncoding.EncodeToString(data),
				"sha": blobOid, "size": len(data), "name": baseName(path), "path": path,
				"url": "/api/v3/repos/" + owner + "/" + name + "/contents/" + path,
			}
			body, _ = json.Marshal(m)
		} else {
			// directory?
			prefix := path + "/"
			isDir := false
			for k := range flat {
				if strings.HasPrefix(k, prefix) {
					isDir = true
					break
				}
			}
			if !isDir {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			body = dirListing(r, flat, prefix)
		}
		store.mu.RUnlock()
		cn.writeJSON(200, body, rq, nil)
	case "PUT":
		m, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		msg := bodyStr(m, "message")
		contentB64 := bodyStr(m, "content")
		data, err := base64.StdEncoding.DecodeString(contentB64)
		if err != nil {
			cn.writeErr(400, "invalid base64 content", rq)
			return
		}
		branch := bodyStr(m, "branch")
		sha := bodyStr(m, "sha")
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if branch == "" {
			branch = r.defaultBranch
		}
		tip, exists := r.refs["refs/heads/"+branch]
		var flat map[string]string
		var parents []string
		if exists {
			flat = flatTree(r, tip)
			parents = []string{tip}
		} else {
			// create branch from default tip, or root commit if repo empty
			defTip := defaultTip(r)
			if defTip != "" {
				flat = flatTree(r, defTip)
				parents = []string{defTip}
			} else {
				flat = map[string]string{}
			}
		}
		_, fileExists := flat[path]
		if fileExists {
			if sha == "" {
				store.mu.Unlock()
				cn.writeErr(422, "sha is required to update a file", rq)
				return
			}
			if flat[path] != sha {
				store.mu.Unlock()
				cn.writeErr(409, "sha does not match", rq)
				return
			}
		} else if sha != "" {
			store.mu.Unlock()
			cn.writeErr(422, "sha does not match", rq)
			return
		}
		blobOid := storeObj(r, objBlob, data)
		flat[path] = blobOid
		treeOid := buildTree(r, flat)
		commitOid := makeCommit(r, treeOid, parents, msg)
		r.refs["refs/heads/"+branch] = commitOid
		r.invalidate()
		status := 201
		if fileExists {
			status = 200
		}
		resp := map[string]any{
			"content": map[string]any{"name": baseName(path), "path": path, "sha": blobOid, "size": len(data), "encoding": "base64"},
			"commit":  map[string]any{"sha": commitOid, "message": msg},
		}
		body, _ := json.Marshal(resp)
		store.mu.Unlock()
		cn.writeJSON(status, body, rq, nil)
	case "DELETE":
		m, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		sha := bodyStr(m, "sha")
		msg := bodyStr(m, "message")
		branch := bodyStr(m, "branch")
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if branch == "" {
			branch = r.defaultBranch
		}
		tip, exists := r.refs["refs/heads/"+branch]
		if !exists {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		flat := flatTree(r, tip)
		cur, ok := flat[path]
		if !ok {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if sha != "" && sha != cur {
			store.mu.Unlock()
			cn.writeErr(409, "sha does not match", rq)
			return
		}
		delete(flat, path)
		treeOid := buildTree(r, flat)
		commitOid := makeCommit(r, treeOid, []string{tip}, msg)
		r.refs["refs/heads/"+branch] = commitOid
		r.invalidate()
		resp := map[string]any{"commit": map[string]any{"sha": commitOid, "message": msg}}
		body, _ := json.Marshal(resp)
		store.mu.Unlock()
		cn.writeJSON(200, body, rq, nil)
	default:
		cn.writeErr(404, "Not Found", rq)
	}
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dirListing(r *Repo, flat map[string]string, prefix string) []byte {
	seen := map[string]bool{}
	var entries []map[string]any
	for k, blobOid := range flat {
		rest := strings.TrimPrefix(k, prefix)
		if rest == k && prefix != "" {
			continue
		}
		seg := rest
		isDir := false
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			seg = rest[:j]
			isDir = true
		}
		if seen[seg] {
			continue
		}
		seen[seg] = true
		if isDir {
			entries = append(entries, map[string]any{"type": "dir", "name": seg, "path": prefix + seg, "sha": ""})
		} else {
			o := r.objects[blobOid]
			size := 0
			if o != nil {
				size = len(o.data)
			}
			entries = append(entries, map[string]any{"type": "file", "name": seg, "path": prefix + seg, "sha": blobOid, "size": size})
		}
	}
	// deterministic order
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j]["name"].(string) < entries[i]["name"].(string) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	if entries == nil {
		entries = []map[string]any{}
	}
	b, _ := json.Marshal(entries)
	return b
}

// ---------- git refs ----------

func handleGitRefs(cn *conn, rq *request, owner, name string, rest []string) {
	if len(rest) == 0 || rest[0] != "refs" {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if rq.method == "POST" && len(rest) == 1 {
		m, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		ref := bodyStr(m, "ref")
		sha := bodyStr(m, "sha")
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if _, ok := r.objects[sha]; !ok {
			store.mu.Unlock()
			cn.writeErr(422, "object does not exist", rq)
			return
		}
		if _, exists := r.refs[ref]; exists {
			store.mu.Unlock()
			cn.writeErr(422, "Reference already exists", rq)
			return
		}
		r.refs[ref] = sha
		r.invalidate()
		resp := map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}, "url": "/api/v3/repos/" + owner + "/" + name + "/git/refs/" + strings.TrimPrefix(ref, "refs/")}
		body, _ := json.Marshal(resp)
		store.mu.Unlock()
		cn.writeJSON(201, body, rq, nil)
		return
	}
	if rq.method == "GET" {
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if len(rest) == 1 {
			var out []map[string]any
			for _, ref := range sortedRefNames(r) {
				out = append(out, map[string]any{"ref": ref, "object": map[string]any{"sha": r.refs[ref], "type": refType(r, r.refs[ref])}})
			}
			if out == nil {
				out = []map[string]any{}
			}
			b, _ := json.Marshal(out)
			store.mu.RUnlock()
			cn.writeJSON(200, b, rq, nil)
			return
		}
		ref := "refs/" + strings.Join(rest[1:], "/")
		sha, ok := r.refs[ref]
		if !ok {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		resp := map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": refType(r, sha)}}
		b, _ := json.Marshal(resp)
		store.mu.RUnlock()
		cn.writeJSON(200, b, rq, nil)
		return
	}
	cn.writeErr(404, "Not Found", rq)
}

func refType(r *Repo, sha string) string {
	o := r.objects[sha]
	if o == nil {
		return "commit"
	}
	return typeNames[o.typ]
}

// ---------- labels ----------

func handleLabels(cn *conn, rq *request, owner, name string, rest []string) {
	if rq.method != "POST" || len(rest) != 0 {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	m, ok := parseBody(rq)
	if !ok {
		cn.writeErr(400, "Problems parsing JSON", rq)
		return
	}
	label := bodyStr(m, "name")
	color := bodyStr(m, "color")
	if label == "" {
		cn.writeErr(422, "name is required", rq)
		return
	}
	store.mu.Lock()
	r := resolveRepo(owner, name)
	if r == nil {
		store.mu.Unlock()
		cn.writeErr(404, "Not Found", rq)
		return
	}
	r.labels[label] = color
	resp := map[string]any{"name": label, "color": color, "description": ""}
	body, _ := json.Marshal(resp)
	store.mu.Unlock()
	cn.writeJSON(201, body, rq, nil)
}

// ---------- issues ----------

func handleIssues(cn *conn, rq *request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch rq.method {
		case "GET":
			store.mu.RLock()
			r := resolveRepo(owner, name)
			if r == nil {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			state := rq.query["state"]
			if state == "" {
				state = "open"
			}
			sortDir := rq.query["direction"]
			if sortDir == "" {
				sortDir = "desc"
			}
			var list []*Issue
			for _, iss := range r.issues {
				if state == "all" || iss.state == state {
					list = append(list, iss)
				}
			}
			if sortDir == "asc" {
				// list is already in creation order
			} else {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			page, link := slicePage(rq, list)
			var b strings.Builder
			b.WriteString("[")
			for i, iss := range page {
				if i > 0 {
					b.WriteString(",")
				}
				b.Write(issueJSON(iss))
			}
			b.WriteString("]")
			var extra map[string]string
			if link != "" {
				extra = map[string]string{"Link": link}
			}
			store.mu.RUnlock()
			cn.writeJSON(200, []byte(b.String()), rq, extra)
		case "POST":
			m, ok := parseBody(rq)
			if !ok {
				cn.writeErr(400, "Problems parsing JSON", rq)
				return
			}
			title := bodyStr(m, "title")
			if title == "" {
				cn.writeErr(422, "title is required", rq)
				return
			}
			store.mu.Lock()
			r := resolveRepo(owner, name)
			if r == nil {
				store.mu.Unlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			var labels []string
			if lv, ok := m["labels"]; ok {
				if arr, ok := lv.([]any); ok {
					for _, l := range arr {
						if s, ok := l.(string); ok {
							if _, exists := r.labels[s]; !exists {
								store.mu.Unlock()
								cn.writeJSON(422, errJSON("Invalid label: "+s), rq, nil)
								return
							}
							labels = append(labels, s)
						}
					}
				}
			}
			store.nextID++
			iss := &Issue{
				repo: r, number: len(r.issues) + 1, id: store.nextID - 1,
				title: title, body: bodyStr(m, "body"), state: "open",
				labels: labels, seq: store.nextSeq(),
			}
			r.issues = append(r.issues, iss)
			r.issuesByNum[iss.number] = iss
			body := issueJSON(iss)
			store.mu.Unlock()
			cn.writeJSON(201, body, rq, nil)
		default:
			cn.writeErr(404, "Not Found", rq)
		}
		return
	}
	// /issues/{n} or /issues/{n}/comments
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		handleIssueComments(cn, rq, owner, name, n)
		return
	}
	if len(rest) != 1 {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	switch rq.method {
	case "GET":
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		iss := r.issuesByNum[n]
		if iss == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		body := issueJSON(iss)
		store.mu.RUnlock()
		cn.writeJSON(200, body, rq, nil)
	case "PATCH":
		m, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		iss := r.issuesByNum[n]
		if iss == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if s := bodyStr(m, "state"); s == "open" || s == "closed" {
			iss.state = s
		}
		if t := bodyStr(m, "title"); m["title"] != nil {
			iss.title = t
		}
		if bdy := bodyStr(m, "body"); m["body"] != nil {
			iss.body = bdy
		}
		iss.jsonCache = nil
		body := issueJSON(iss)
		store.mu.Unlock()
		cn.writeJSON(200, body, rq, nil)
	default:
		cn.writeErr(404, "Not Found", rq)
	}
}

func handleIssueComments(cn *conn, rq *request, owner, name string, n int) {
	switch rq.method {
	case "GET":
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		iss := r.issuesByNum[n]
		if iss == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		var out []map[string]any
		for _, c := range iss.comments {
			out = append(out, commentJSON(r, iss, c))
		}
		if out == nil {
			out = []map[string]any{}
		}
		b, _ := json.Marshal(out)
		store.mu.RUnlock()
		cn.writeJSON(200, b, rq, nil)
	case "POST":
		m, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		bodyText := bodyStr(m, "body")
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		iss := r.issuesByNum[n]
		if iss == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		store.nextID++
		c := &Comment{id: store.nextID - 1, body: bodyText}
		iss.comments = append(iss.comments, c)
		iss.jsonCache = nil
		b, _ := json.Marshal(commentJSON(r, iss, c))
		store.mu.Unlock()
		cn.writeJSON(201, b, rq, nil)
	default:
		cn.writeErr(404, "Not Found", rq)
	}
}

func commentJSON(r *Repo, iss *Issue, c *Comment) map[string]any {
	return map[string]any{
		"id": c.id, "node_id": b64id("comment", r.id, iss.number, c.id),
		"body": c.body,
		"user": map[string]any{"login": r.ownerLogin(), "type": "User"},
		"created_at": fixedTS, "updated_at": fixedTS,
		"html_url": "https://example.invalid/" + r.ownerLogin() + "/" + r.name + "/issues/" + strconv.Itoa(iss.number) + "#issuecomment-" + strconv.Itoa(c.id),
		"url": "/api/v3/repos/" + r.ownerLogin() + "/" + r.name + "/issues/comments/" + strconv.Itoa(c.id),
	}
}

// ---------- pulls ----------

func prMergeable(r *Repo, pr *Pull) bool {
	if pr.merged || pr.state != "open" {
		return false
	}
	baseTip := r.refs["refs/heads/"+pr.baseRef]
	if baseTip == pr.baseAtCreate {
		return true
	}
	if baseTip == "" {
		return false
	}
	baseFlat := flatTree(r, baseTip)
	headFlat := flatTree(r, pr.headSha)
	baseAtFlat := flatTree(r, pr.baseAtCreate)
	changedBase := diffFlat(baseAtFlat, baseFlat)
	changedHead := diffFlat(baseAtFlat, headFlat)
	headMap := map[string]string{}
	for _, f := range changedHead {
		if f.status == "removed" {
			headMap[f.filename] = ""
		} else {
			headMap[f.filename] = headFlat[f.filename]
		}
	}
	for _, f := range changedBase {
		if hv, ok := headMap[f.filename]; ok {
			var bv string
			if f.status != "removed" {
				bv = baseFlat[f.filename]
			}
			if hv != bv {
				return false
			}
		}
	}
	return true
}

func pullJSON(r *Repo, pr *Pull) []byte {
	owner := r.ownerLogin()
	m := map[string]any{
		"id": pr.id, "node_id": b64id("pr", r.id, pr.number),
		"number": pr.number, "title": pr.title, "body": pr.body,
		"state": pr.state,
		"head": map[string]any{"ref": pr.headRef, "sha": pr.headSha, "repo": map[string]any{"name": r.name, "full_name": owner + "/" + r.name}},
		"base": map[string]any{"ref": pr.baseRef, "sha": pr.baseSha, "repo": map[string]any{"name": r.name, "full_name": owner + "/" + r.name}},
		"user": map[string]any{"login": owner, "type": "User"},
		"merged": pr.merged, "mergeable": prMergeable(r, pr),
		"merged_at": nil, "merge_commit_sha": nil, "closed_at": nil,
		"created_at": fixedTS, "updated_at": fixedTS,
		"comments": len(pr.comments),
		"html_url": "https://example.invalid/" + owner + "/" + r.name + "/pull/" + strconv.Itoa(pr.number),
		"url": "/api/v3/repos/" + owner + "/" + r.name + "/pulls/" + strconv.Itoa(pr.number),
	}
	if pr.merged {
		m["merged_at"] = pr.mergedAt
		m["merge_commit_sha"] = pr.mergeSha
		m["closed_at"] = pr.closedAt
	} else if pr.state == "closed" {
		m["closed_at"] = pr.closedAt
	}
	b, _ := json.Marshal(m)
	return b
}

func handlePulls(cn *conn, rq *request, owner, name string, rest []string) {
	if len(rest) == 0 {
		switch rq.method {
		case "GET":
			store.mu.RLock()
			r := resolveRepo(owner, name)
			if r == nil {
				store.mu.RUnlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			state := rq.query["state"]
			if state == "" {
				state = "open"
			}
			var list []*Pull
			for _, pr := range r.pulls {
				if state == "all" || pr.state == state {
					list = append(list, pr)
				}
			}
			page, link := slicePage(rq, list)
			var b strings.Builder
			b.WriteString("[")
			for i, pr := range page {
				if i > 0 {
					b.WriteString(",")
				}
				b.Write(pullJSON(r, pr))
			}
			b.WriteString("]")
			var extra map[string]string
			if link != "" {
				extra = map[string]string{"Link": link}
			}
			store.mu.RUnlock()
			cn.writeJSON(200, []byte(b.String()), rq, extra)
		case "POST":
			m, ok := parseBody(rq)
			if !ok {
				cn.writeErr(400, "Problems parsing JSON", rq)
				return
			}
			title := bodyStr(m, "title")
			head := bodyStr(m, "head")
			base := bodyStr(m, "base")
			if title == "" || head == "" || base == "" {
				cn.writeErr(422, "title, head and base are required", rq)
				return
			}
			store.mu.Lock()
			r := resolveRepo(owner, name)
			if r == nil {
				store.mu.Unlock()
				cn.writeErr(404, "Not Found", rq)
				return
			}
			headSha, ok := r.refs["refs/heads/"+head]
			if !ok {
				store.mu.Unlock()
				cn.writeErr(422, "head branch does not exist", rq)
				return
			}
			baseSha, ok := r.refs["refs/heads/"+base]
			if !ok {
				store.mu.Unlock()
				cn.writeErr(422, "base branch does not exist", rq)
				return
			}
			pr, _ := createPullLocked(r, title, bodyStr(m, "body"), head, base, headSha, baseSha)
			body := pullJSON(r, pr)
			store.mu.Unlock()
			cn.writeJSON(201, body, rq, nil)
		default:
			cn.writeErr(404, "Not Found", rq)
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if len(rest) == 2 && rest[1] == "files" {
		if rq.method != "GET" {
			cn.writeErr(404, "Not Found", rq)
			return
		}
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		pr := r.pullsByNum[n]
		if pr == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		var out []map[string]any
		for _, f := range pr.files {
			out = append(out, map[string]any{"filename": f.filename, "status": fileStatus(f.status), "additions": f.additions, "deletions": f.deletions, "changes": f.additions + f.deletions})
		}
		if out == nil {
			out = []map[string]any{}
		}
		b, _ := json.Marshal(out)
		store.mu.RUnlock()
		cn.writeJSON(200, b, rq, nil)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" {
		if rq.method != "PUT" {
			cn.writeErr(404, "Not Found", rq)
			return
		}
		_, ok := parseBody(rq)
		if !ok {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		store.mu.Lock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		pr := r.pullsByNum[n]
		if pr == nil {
			store.mu.Unlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		if pr.merged || pr.state != "open" {
			store.mu.Unlock()
			cn.writeJSON(405, errJSON("Pull request is already closed"), rq, nil)
			return
		}
		if !prMergeable(r, pr) {
			store.mu.Unlock()
			cn.writeJSON(405, errJSON("Merge conflict"), rq, nil)
			return
		}
		mergeSha := mergePullLocked(r, pr)
		resp := map[string]any{"sha": mergeSha, "merged": true, "message": "Pull request successfully merged"}
		body, _ := json.Marshal(resp)
		store.mu.Unlock()
		cn.writeJSON(200, body, rq, nil)
		return
	}
	if len(rest) == 1 {
		if rq.method != "GET" {
			cn.writeErr(404, "Not Found", rq)
			return
		}
		store.mu.RLock()
		r := resolveRepo(owner, name)
		if r == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		pr := r.pullsByNum[n]
		if pr == nil {
			store.mu.RUnlock()
			cn.writeErr(404, "Not Found", rq)
			return
		}
		body := pullJSON(r, pr)
		store.mu.RUnlock()
		cn.writeJSON(200, body, rq, nil)
		return
	}
	cn.writeErr(404, "Not Found", rq)
}

func fileStatus(s string) string {
	switch s {
	case "added":
		return "added"
	case "removed":
		return "removed"
	default:
		return "modified"
	}
}

// createPullLocked creates a pull; caller holds the write lock.
func createPullLocked(r *Repo, title, body, head, base, headSha, baseSha string) (*Pull, bool) {
	store.nextID++
	pr := &Pull{
		repo: r, number: len(r.pulls) + 1, id: store.nextID - 1,
		title: title, body: body, headRef: head, baseRef: base,
		headSha: headSha, baseSha: baseSha, baseAtCreate: baseSha,
		state: "open", seq: store.nextSeq(),
	}
	// files: diff baseAtCreate -> head
	baseFlat := flatTree(r, baseSha)
	headFlat := flatTree(r, headSha)
	changes := diffFlat(baseFlat, headFlat)
	for _, c := range changes {
		if c.status == "modified" || c.status == "added" {
			c.additions = 1
		}
		if c.status == "modified" || c.status == "removed" {
			c.deletions = 1
		}
		if c.status == "modified" {
			c.status = "changed"
		}
		pr.files = append(pr.files, c)
	}
	r.pulls = append(r.pulls, pr)
	r.pullsByNum[pr.number] = pr
	return pr, true
}

// mergePullLocked applies a squash merge; caller holds the write lock.
func mergePullLocked(r *Repo, pr *Pull) string {
	baseTip := r.refs["refs/heads/"+pr.baseRef]
	baseFlat := flatTree(r, baseTip)
	headFlat := flatTree(r, pr.headSha)
	baseAtFlat := flatTree(r, pr.baseAtCreate)
	merged := map[string]string{}
	for k, v := range baseFlat {
		merged[k] = v
	}
	for k, v := range diffFlat(baseAtFlat, headFlat) {
		_ = v
		_ = k
	}
	// apply head changes onto base: for every path in head flat that differs
	// from baseAt, take head's version; paths deleted in head are removed.
	for path, oid := range headFlat {
		if baseAtFlat[path] != oid {
			merged[path] = oid
		}
	}
	for path := range baseAtFlat {
		if _, ok := headFlat[path]; !ok {
			delete(merged, path)
		}
	}
	treeOid := buildTree(r, merged)
	mergeSha := makeCommit(r, treeOid, []string{baseTip}, pr.title)
	r.refs["refs/heads/"+pr.baseRef] = mergeSha
	pr.state = "closed"
	pr.merged = true
	pr.mergeSha = mergeSha
	pr.mergedAt = fixedTS
	pr.closedAt = fixedTS
	r.invalidate()
	return mergeSha
}
