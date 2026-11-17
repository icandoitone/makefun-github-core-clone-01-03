// MCP endpoint: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

var toolsListJSON []byte
var toolsListOnce sync.Once

func buildToolsList() []byte {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	tools := []any{
		map[string]any{
			"name": "create_repo", "description": "Create a new repository owned by the authenticated user.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"name": str("Name of the repository to create."),
				"description": str("A short description of the repository."),
				"private": map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			}, "required": []string{"name"}},
		},
		map[string]any{
			"name": "get_repo", "description": "Get a repository by owner and name.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"owner": str("The login of the repository owner."),
				"repo":  str("The name of the repository."),
			}, "required": []string{"owner", "repo"}},
		},
		map[string]any{
			"name": "create_issue", "description": "Create an issue in a repository.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"owner": str("The login of the repository owner."),
				"repo":  str("The name of the repository."),
				"title": str("The title of the issue."),
				"body":  str("The body of the issue."),
			}, "required": []string{"owner", "repo", "title"}},
		},
		map[string]any{
			"name": "list_issues", "description": "List issues in a repository.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"owner": str("The login of the repository owner."),
				"repo":  str("The name of the repository."),
				"state": str("Filter by state: open, closed, or all."),
			}, "required": []string{"owner", "repo"}},
		},
		map[string]any{
			"name": "create_pull_request", "description": "Create a pull request in a repository.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"owner": str("The login of the repository owner."),
				"repo":  str("The name of the repository."),
				"title": str("The title of the pull request."),
				"head":  str("The name of the branch with the changes."),
				"base":  str("The name of the branch to merge into."),
				"body":  str("The body of the pull request."),
			}, "required": []string{"owner", "repo", "title", "head", "base"}},
		},
		map[string]any{
			"name": "merge_pull_request", "description": "Merge a pull request.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"owner":       str("The login of the repository owner."),
				"repo":        str("The name of the repository."),
				"pull_number": map[string]any{"type": "integer", "description": "The number of the pull request to merge."},
			}, "required": []string{"owner", "repo", "pull_number"}},
		},
	}
	b, _ := json.Marshal(map[string]any{"tools": tools})
	return b
}

func getToolsList() []byte {
	toolsListOnce.Do(func() { toolsListJSON = buildToolsList() })
	return toolsListJSON
}

func handleMCP(cn *conn, rq *request) {
	switch rq.method {
	case "DELETE":
		cn.writeRaw(204, "", nil, nil, rq)
		return
	case "GET":
		cn.writeJSON(405, errJSON("Method not allowed"), rq, nil)
		return
	case "POST":
	default:
		cn.writeJSON(405, errJSON("Method not allowed"), rq, nil)
		return
	}
	if !authed(rq) {
		cn.writeJSON(401, errJSON("Requires authentication"), rq, nil)
		return
	}
	var msg map[string]any
	if err := json.Unmarshal(rq.body, &msg); err != nil {
		cn.writeJSON(400, rpcError(nil, -32700, "Parse error"), rq, nil)
		return
	}
	id, hasID := msg["id"]
	method, _ := msg["method"].(string)
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	// Notifications (no id) are acknowledged with 202 and an empty body.
	if !hasID || id == nil {
		cn.writeRaw(202, "", nil, nil, rq)
		return
	}
	var extra map[string]string
	if sid := rq.headers["mcp-session-id"]; sid != "" {
		extra = map[string]string{"Mcp-Session-Id": sid}
	}
	switch method {
	case "server/discover":
		result := map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25"},
			"protocolVersion":   "2026-07-28",
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
		}
		cn.writeJSON(200, rpcResult(id, result), rq, extra)
	case "initialize":
		want := "2025-11-25"
		if pv, ok := params["protocolVersion"].(string); ok && pv != "" {
			// echo a 2025-11-25-or-earlier revision
			if pv <= "2025-11-25" {
				want = pv
			}
		}
		result := map[string]any{
			"protocolVersion": want,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		}
		cn.writeJSON(200, rpcResult(id, result), rq, extra)
	case "ping":
		cn.writeJSON(200, rpcResult(id, map[string]any{}), rq, extra)
	case "tools/list":
		// Precomputed bytes: identical on every call. The envelope differs
		// only in the request id.
		var b strings.Builder
		b.WriteString(`{"jsonrpc":"2.0","id":`)
		idb, _ := json.Marshal(id)
		b.Write(idb)
		b.WriteString(`,"result":`)
		b.Write(getToolsList())
		b.WriteString(`}`)
		cn.writeJSON(200, []byte(b.String()), rq, extra)
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		cn.writeJSON(200, mcpCall(id, name, args), rq, extra)
	default:
		cn.writeJSON(200, rpcError(id, -32601, "Method not found"), rq, extra)
	}
}

func rpcResult(id any, result any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

func rpcError(id any, code int, message string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	return b
}

// ---------- tool argument helpers ----------

func mcpStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func mcpInt(args map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch n := v.(type) {
			case float64:
				return int(n), true
			case int:
				return n, true
			case string:
				if i, err := strconv.Atoi(n); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func mcpBool(args map[string]any, k string) bool {
	if v, ok := args[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func matchTool(name, canonical string) bool {
	l := strings.ToLower(name)
	if l == canonical {
		return true
	}
	for _, sep := range []string{"_", "/", ":", "-"} {
		if strings.HasSuffix(l, sep+canonical) {
			return true
		}
	}
	return false
}

func toolResult(id any, obj any) []byte {
	text, _ := json.Marshal(obj)
	return toolResultText(id, obj, text)
}

func toolResultText(id any, obj any, text []byte) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content":          []any{map[string]any{"type": "text", "text": string(text)}},
			"structuredContent": obj,
			"isError":          false,
		},
	})
	return b
}

func toolError(id any, msg string) []byte {
	return toolErrorObj(id, map[string]any{"error": msg})
}

func toolErrorObj(id any, obj any) []byte {
	text, _ := json.Marshal(obj)
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"result": map[string]any{
			"content":          []any{map[string]any{"type": "text", "text": string(text)}},
			"structuredContent": obj,
			"isError":          true,
		},
	})
	return b
}

func mcpCall(id any, name string, args map[string]any) []byte {
	owner := mcpStr(args, "owner", "owner_login", "ownerLogin")
	repoName := mcpStr(args, "repo", "repository", "repo_name", "repository_name", "name")
	switch {
	case matchTool(name, "create_repo"):
		repoName = mcpStr(args, "name", "repo", "repository")
		if repoName == "" {
			return rpcError(id, -32602, "missing required argument: name")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, exists := store.repos[repoName]; exists {
			return toolError(id, "Repository already exists")
		}
		r := store.createRepo(repoName, mcpStr(args, "description"), mcpBool(args, "private"))
		var obj map[string]any
		_ = json.Unmarshal(repoJSON(r), &obj)
		return toolResult(id, obj)
	case matchTool(name, "get_repo"):
		if owner == "" || repoName == "" {
			return rpcError(id, -32602, "missing required argument: owner/repo")
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			return toolError(id, "Repository not found")
		}
		var obj map[string]any
		_ = json.Unmarshal(repoJSON(r), &obj)
		return toolResult(id, obj)
	case matchTool(name, "create_issue"):
		title := mcpStr(args, "title", "issue_title")
		if owner == "" || repoName == "" || title == "" {
			return rpcError(id, -32602, "missing required argument: owner/repo/title")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			return toolError(id, "Repository not found")
		}
		store.nextID++
		iss := &Issue{repo: r, number: len(r.issues) + 1, id: store.nextID - 1, title: title, body: mcpStr(args, "body", "issue_body"), state: "open", seq: store.nextSeq()}
		r.issues = append(r.issues, iss)
		r.issuesByNum[iss.number] = iss
		var obj map[string]any
		_ = json.Unmarshal(issueJSON(iss), &obj)
		return toolResult(id, obj)
	case matchTool(name, "list_issues"):
		if owner == "" || repoName == "" {
			return rpcError(id, -32602, "missing required argument: owner/repo")
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			return toolError(id, "Repository not found")
		}
		state := mcpStr(args, "state")
		if state == "" {
			state = "open"
		}
		var arr []any
		for _, iss := range r.issues {
			if state == "all" || iss.state == state {
				var o map[string]any
				_ = json.Unmarshal(issueJSON(iss), &o)
				arr = append(arr, o)
			}
		}
		if arr == nil {
			arr = []any{}
		}
		return toolResult(id, map[string]any{"issues": arr})
	case matchTool(name, "create_pull_request"):
		title := mcpStr(args, "title")
		head := mcpStr(args, "head", "head_ref", "headRef", "head_branch")
		base := mcpStr(args, "base", "base_ref", "baseRef", "base_branch")
		if owner == "" || repoName == "" || title == "" || head == "" || base == "" {
			return rpcError(id, -32602, "missing required argument: owner/repo/title/head/base")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			return toolError(id, "Repository not found")
		}
		headSha, ok := r.refs["refs/heads/"+head]
		if !ok {
			return toolError(id, "head branch does not exist")
		}
		baseSha, ok := r.refs["refs/heads/"+base]
		if !ok {
			return toolError(id, "base branch does not exist")
		}
		pr, _ := createPullLocked(r, title, mcpStr(args, "body"), head, base, headSha, baseSha)
		var obj map[string]any
		_ = json.Unmarshal(pullJSON(r, pr), &obj)
		return toolResult(id, obj)
	case matchTool(name, "merge_pull_request"):
		n, ok := mcpInt(args, "pull_number", "pullNumber", "number")
		if owner == "" || repoName == "" || !ok {
			return rpcError(id, -32602, "missing required argument: owner/repo/pull_number")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		r := store.repoByName(owner, repoName)
		if r == nil {
			return toolError(id, "Repository not found")
		}
		pr := r.pullsByNum[n]
		if pr == nil {
			return toolError(id, "Pull request not found")
		}
		if pr.merged || pr.state != "open" {
			return toolError(id, "Pull request is already closed")
		}
		if !prMergeable(r, pr) {
			return toolError(id, "Merge conflict")
		}
		mergeSha := mergePullLocked(r, pr)
		return toolResult(id, map[string]any{"sha": mergeSha, "merged": true, "message": "Pull request successfully merged"})
	default:
		return rpcError(id, -32602, "unknown tool: "+name)
	}
}
