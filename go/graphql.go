// GraphQL endpoint: hand-rolled parser and executor, stdlib only.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---------- tokenizer ----------

type gqlToken struct {
	kind string // "name", "string", "int", "punct", "var", "spread"
	val  string
}

func tokenizeGQL(src string) ([]gqlToken, error) {
	var toks []gqlToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '!' || c == '=' || c == '@':
			toks = append(toks, gqlToken{kind: "punct", val: string(c)})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlToken{kind: "spread", val: "..."})
			i += 3
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlToken{kind: "var", val: src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlToken{kind: "string", val: sb.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '-' || src[j] == '+') {
				j++
			}
			toks = append(toks, gqlToken{kind: "int", val: src[i:j]})
			i = j
		case isNameStart(c):
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			toks = append(toks, gqlToken{kind: "name", val: src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return toks, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// ---------- parser ----------

type gqlField struct {
	alias    string
	name     string
	args     map[string]any
	children []gqlField
	fragOn   string // for inline fragments: type condition ("" = none)
	fragName string // named fragment spread
}

type gqlOperation struct {
	kind     string // "query" | "mutation"
	name     string
	varDefs  map[string]any // var name -> default value (nil if none)
	fields   []gqlField
}

type gqlParser struct {
	toks []gqlToken
	pos  int
	frags map[string][]gqlField
}

func (p *gqlParser) peek() *gqlToken {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *gqlParser) next() *gqlToken {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

func (p *gqlParser) expectPunct(v string) error {
	t := p.next()
	if t == nil || t.kind != "punct" || t.val != v {
		return fmt.Errorf("expected %s", v)
	}
	return nil
}

func (p *gqlParser) parseDocument() ([]gqlOperation, error) {
	var ops []gqlOperation
	for p.peek() != nil {
		t := p.peek()
		if t.kind == "name" && t.val == "fragment" {
			p.next()
			nameTok := p.next()
			if nameTok == nil {
				return nil, fmt.Errorf("bad fragment")
			}
			// "on" Type
			p.next() // "on"
			p.next() // type name
			fields, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			p.frags[nameTok.val] = fields
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (gqlOperation, error) {
	var op gqlOperation
	op.kind = "query"
	op.varDefs = map[string]any{}
	t := p.peek()
	if t != nil && t.kind == "name" && (t.val == "query" || t.val == "mutation") {
		op.kind = t.val
		p.next()
		if nt := p.peek(); nt != nil && nt.kind == "name" {
			op.name = nt.val
			p.next()
		}
	}
	// variable definitions
	if t := p.peek(); t != nil && t.kind == "punct" && t.val == "(" {
		p.next()
		for {
			t := p.peek()
			if t == nil {
				return op, fmt.Errorf("bad var defs")
			}
			if t.kind == "punct" && t.val == ")" {
				p.next()
				break
			}
			if t.kind == "var" {
				p.next()
				name := t.val
				// ":" Type [!] [= default]; type is [Name!]! etc.
				p.expectPunct(":")
				if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "[" {
					p.next()
				}
				nt := p.next()
				if nt == nil || nt.kind != "name" {
					return op, fmt.Errorf("bad var type")
				}
				if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "!" {
					p.next()
				}
				if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "]" {
					p.next()
				}
				if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "!" {
					p.next()
				}
				var def any
				if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "=" {
					p.next()
					v, err := p.parseValue()
					if err != nil {
						return op, err
					}
					def = v
				}
				op.varDefs[name] = def
				continue
			}
			return op, fmt.Errorf("unexpected token in var defs")
		}
	}
	fields, err := p.parseSelectionSet()
	if err != nil {
		return op, err
	}
	op.fields = fields
	return op, nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var fields []gqlField
	for {
		t := p.peek()
		if t == nil {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if t.kind == "punct" && t.val == "}" {
			p.next()
			break
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f...)
	}
	return fields, nil
}

func (p *gqlParser) parseField() ([]gqlField, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("unexpected end")
	}
	if t.kind == "spread" {
		p.next()
		nt := p.peek()
		if nt != nil && nt.kind == "name" && nt.val == "on" {
			p.next()
			typeTok := p.next()
			fields, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			for i := range fields {
				fields[i].fragOn = typeTok.val
			}
			return fields, nil
		}
		if nt != nil && nt.kind == "name" {
			// named fragment spread
			name := nt.val
			p.next()
			if frag, ok := p.frags[name]; ok {
				return frag, nil
			}
			return []gqlField{{fragName: name}}, nil
		}
		// ... { ... }
		fields, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return fields, nil
	}
	if t.kind != "name" {
		return nil, fmt.Errorf("expected field name")
	}
	name := t.val
	p.next()
	alias := ""
	if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == ":" {
		p.next()
		alias = name
		nameTok := p.next()
		if nameTok == nil || nameTok.kind != "name" {
			return nil, fmt.Errorf("expected field name after alias")
		}
		name = nameTok.val
	}
	f := gqlField{name: name, alias: alias, args: map[string]any{}}
	// arguments
	if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "(" {
		p.next()
		for {
			at := p.peek()
			if at == nil {
				return nil, fmt.Errorf("bad arguments")
			}
			if at.kind == "punct" && at.val == ")" {
				p.next()
				break
			}
			if at.kind != "name" {
				return nil, fmt.Errorf("expected argument name")
			}
			argName := at.val
			p.next()
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			f.args[argName] = v
		}
	}
	// directives: skip @name(args)
	for {
		nt := p.peek()
		if nt != nil && nt.kind == "punct" && nt.val == "@" {
			p.next()
			p.next() // directive name
			if dt := p.peek(); dt != nil && dt.kind == "punct" && dt.val == "(" {
				depth := 0
				for {
					tk := p.next()
					if tk == nil {
						break
					}
					if tk.kind == "punct" && tk.val == "(" {
						depth++
					}
					if tk.kind == "punct" && tk.val == ")" {
						depth--
						if depth == 0 {
							break
						}
					}
				}
			}
			continue
		}
		break
	}
	// sub-selection
	if nt := p.peek(); nt != nil && nt.kind == "punct" && nt.val == "{" {
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
	}
	return []gqlField{f}, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	if t == nil {
		return nil, fmt.Errorf("unexpected end in value")
	}
	switch t.kind {
	case "var":
		return varRef{t.val}, nil
	case "string":
		return t.val, nil
	case "int":
		if strings.ContainsAny(t.val, ".eE") {
			f, err := strconv.ParseFloat(t.val, 64)
			if err != nil {
				return nil, err
			}
			return f, nil
		}
		n, err := strconv.Atoi(t.val)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "name":
		switch t.val {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return enumVal(t.val), nil
		}
	case "punct":
		if t.val == "[" {
			var out []any
			for {
				nt := p.peek()
				if nt == nil {
					return nil, fmt.Errorf("unterminated list")
				}
				if nt.kind == "punct" && nt.val == "]" {
					p.next()
					break
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		if t.val == "{" {
			out := map[string]any{}
			for {
				nt := p.peek()
				if nt == nil {
					return nil, fmt.Errorf("unterminated object")
				}
				if nt.kind == "punct" && nt.val == "}" {
					p.next()
					break
				}
				if nt.kind != "name" {
					return nil, fmt.Errorf("expected object key")
				}
				k := nt.val
				p.next()
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				out[k] = v
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token")
}

type varRef struct{ name string }
type enumVal string

// resolveVars substitutes variable references using provided values/defaults.
func resolveVars(v any, vars map[string]any, defs map[string]any) any {
	switch x := v.(type) {
	case varRef:
		if val, ok := vars[x.name]; ok {
			return val
		}
		if def, ok := defs[x.name]; ok {
			return def
		}
		return nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = resolveVars(e, vars, defs)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = resolveVars(e, vars, defs)
		}
		return out
	case enumVal:
		return string(x)
	default:
		return v
	}
}

func argStr(args map[string]any, k string) string {
	if v, ok := args[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argInt(args map[string]any, k string, def int) int {
	if v, ok := args[k]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return int(i)
			}
		}
	}
	return def
}

// ---------- introspection data ----------

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "url", "createdAt"},
	"PullRequest": {"id", "number", "title", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "url", "createdAt"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "url", "createdAt"},
	"User":        {"id", "login", "name"},
}

func typeIntrospection(name string) map[string]any {
	if name == "SearchType" {
		return map[string]any{
			"name": name, "kind": "ENUM",
			"enumValues": []any{
				map[string]any{"name": "ISSUE"},
				map[string]any{"name": "REPOSITORY"},
				map[string]any{"name": "USER"},
			},
		}
	}
	fields, ok := gqlTypeFields[name]
	if !ok {
		return nil
	}
	var fl []any
	for _, f := range fields {
		fl = append(fl, map[string]any{"name": f, "type": map[string]any{"name": "String", "kind": "SCALAR"}})
	}
	return map[string]any{"name": name, "kind": "OBJECT", "fields": fl}
}

// ---------- executor ----------

type gqlCtx struct {
	vars map[string]any
	defs map[string]any
	errs []map[string]any
}

func (c *gqlCtx) errf(format string, args ...any) {
	c.errs = append(c.errs, map[string]any{"message": fmt.Sprintf(format, args...)})
}

func decodeGlobalID(id string) (kind string, nums []int) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil
	}
	kind = parts[0]
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil {
			return "", nil
		}
		nums = append(nums, n)
	}
	return kind, nums
}

func handleGraphQL(cn *conn, rq *request) {
	if rq.method != "POST" {
		cn.writeErr(404, "Not Found", rq)
		return
	}
	if !authed(rq) {
		cn.writeJSON(401, errJSON("Requires authentication"), rq, nil)
		return
	}
	var raw json.RawMessage
	trimmed := strings.TrimSpace(string(rq.body))
	if strings.HasPrefix(trimmed, "[") {
		// batch
		var arr []json.RawMessage
		if err := json.Unmarshal(rq.body, &arr); err != nil {
			cn.writeErr(400, "Problems parsing JSON", rq)
			return
		}
		var results []any
		for _, item := range arr {
			results = append(results, execGraphQLOp(item))
		}
		b, _ := json.Marshal(results)
		cn.writeJSON(200, b, rq, nil)
		return
	}
	_ = raw
	result := execGraphQLOp(rq.body)
	b, _ := json.Marshal(result)
	cn.writeJSON(200, b, rq, nil)
}

func execGraphQLOp(body []byte) map[string]any {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
		Operation string         `json:"operationName"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": "Problems parsing JSON"}}}
	}
	toks, err := tokenizeGQL(req.Query)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	parser := &gqlParser{toks: toks, frags: map[string][]gqlField{}}
	ops, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	if len(ops) == 0 {
		return map[string]any{"errors": []any{map[string]any{"message": "empty document"}}}
	}
	op := ops[0]
	if req.Operation != "" {
		for _, o := range ops {
			if o.name == req.Operation {
				op = o
				break
			}
		}
	}
	if req.Variables == nil {
		req.Variables = map[string]any{}
	}
	ctx := &gqlCtx{vars: req.Variables, defs: op.varDefs}
	// resolve field args now (variables)
	for i := range op.fields {
		resolveFieldArgs(&op.fields[i], ctx)
	}
	if op.kind == "mutation" {
		store.mu.Lock()
		defer store.mu.Unlock()
		data := execFields(ctx, "Query", nil, op.fields)
		return gqlResult(ctx, data)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	data := execFields(ctx, "Query", nil, op.fields)
	return gqlResult(ctx, data)
}

func resolveFieldArgs(f *gqlField, ctx *gqlCtx) {
	for k, v := range f.args {
		f.args[k] = resolveVars(v, ctx.vars, ctx.defs)
	}
	for i := range f.children {
		resolveFieldArgs(&f.children[i], ctx)
	}
}

func gqlResult(ctx *gqlCtx, data map[string]any) map[string]any {
	out := map[string]any{"data": data}
	if len(ctx.errs) > 0 {
		var errs []any
		for _, e := range ctx.errs {
			errs = append(errs, e)
		}
		out["errors"] = errs
	}
	return out
}

func outKey(f gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

// execFields resolves a selection set against a parent object of the given
// GraphQL type. parent is *Repo, *Issue, *Pull, or nil for Query.
func execFields(ctx *gqlCtx, typeName string, parent any, fields []gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" && f.fragOn != typeName {
			continue
		}
		if f.fragName != "" {
			continue
		}
		v := resolveField(ctx, typeName, parent, f)
		out[outKey(f)] = v
	}
	return out
}

func resolveField(ctx *gqlCtx, typeName string, parent any, f gqlField) any {
	// Generic map-backed types (introspection __Type): resolve a present key.
	if m, ok := parent.(map[string]any); ok {
		if v, exists := m[f.name]; exists {
			return resolveValue(ctx, typeName, v, f)
		}
		// otherwise fall through to the type switch (payload carriers)
	}
	switch typeName {
	case "Query":
		return resolveQueryField(ctx, f)
	case "Repository":
		return resolveRepoField(ctx, parent.(*Repo), f)
	case "Issue":
		return resolveIssueField(ctx, parent.(*Issue), f)
	case "PullRequest":
		return resolvePRField(ctx, parent.(*Pull), f)
	case "User":
		return resolveUserField(ctx, parent, f)
	case "Ref":
		if f.name == "name" {
			if s, ok := parent.(string); ok {
				return s
			}
		}
	case "PullRequestFile":
		if f.name == "path" {
			if s, ok := parent.(string); ok {
				return s
			}
		}
	case "Comment":
		if cn, ok := parent.(*commentNode); ok {
			switch f.name {
			case "id":
				return b64id("comment", cn.r.id, cn.iss.number, cn.c.id)
			case "body":
				return cn.c.body
			}
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if f.name == "issue" {
			return execFields(ctx, "Issue", parent.(*Issue), f.children)
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			return execFields(ctx, "PullRequest", parent.(*Pull), f.children)
		}
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			return execFields(ctx, "CommentEdge", parent, f.children)
		}
	case "CommentEdge":
		if f.name == "node" {
			if m, ok := parent.(map[string]any); ok {
				return execFields(ctx, "Comment", &commentNode{c: m["comment"].(*Comment), r: m["repo"].(*Repo), iss: m["issue"].(*Issue)}, f.children)
			}
		}
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, typeName)
	return nil
}

type commentNode struct {
	c   *Comment
	r   *Repo
	iss *Issue
}

// resolveValue resolves a map value against a field's sub-selection.
func resolveValue(ctx *gqlCtx, typeName string, v any, f gqlField) any {
	switch x := v.(type) {
	case map[string]any:
		if len(f.children) == 0 {
			return x
		}
		return execFields(ctx, typeName, x, f.children)
	case []any:
		if len(f.children) == 0 {
			return x
		}
		out := make([]any, 0, len(x))
		for _, e := range x {
			if em, ok := e.(map[string]any); ok {
				out = append(out, execFields(ctx, typeName, em, f.children))
			} else {
				out = append(out, e)
			}
		}
		return out
	default:
		return v
	}
}

func resolveQueryField(ctx *gqlCtx, f gqlField) any {
	switch f.name {
	case "__type":
		name := argStr(f.args, "name")
		ti := typeIntrospection(name)
		if ti == nil {
			return nil
		}
		return execFields(ctx, "__Type", ti, f.children)
	case "__schema":
		return nil
	case "viewer":
		return execFields(ctx, "User", nil, f.children)
	case "repository":
		owner := argStr(f.args, "owner")
		name := argStr(f.args, "name")
		r := store.repoByName(owner, name)
		if r == nil {
			return nil
		}
		return execFields(ctx, "Repository", r, f.children)
	case "node":
		id := argStr(f.args, "id")
		kind, nums := decodeGlobalID(id)
		switch kind {
		case "repo":
			if len(nums) == 1 {
				if r := store.repoByID[nums[0]]; r != nil {
					return execFields(ctx, "Repository", r, f.children)
				}
			}
		case "issue":
			if len(nums) == 2 {
				if r := store.repoByID[nums[0]]; r != nil {
					if iss := r.issuesByNum[nums[1]]; iss != nil {
						return execFields(ctx, "Issue", iss, f.children)
					}
				}
			}
		case "pr":
			if len(nums) == 2 {
				if r := store.repoByID[nums[0]]; r != nil {
					if pr := r.pullsByNum[nums[1]]; pr != nil {
						return execFields(ctx, "PullRequest", pr, f.children)
					}
				}
			}
		}
		return nil
	case "createIssue":
		input, _ := f.args["input"].(map[string]any)
		repoIDStr, _ := input["repositoryId"].(string)
		kind, nums := decodeGlobalID(repoIDStr)
		if kind != "repo" || len(nums) != 1 {
			ctx.errf("Invalid repositoryId")
			return nil
		}
		r := store.repoByID[nums[0]]
		if r == nil {
			ctx.errf("Repository not found")
			return nil
		}
		title, _ := input["title"].(string)
		bodyText, _ := input["body"].(string)
		store.nextID++
		iss := &Issue{repo: r, number: len(r.issues) + 1, id: store.nextID - 1, title: title, body: bodyText, state: "open", seq: store.nextSeq()}
		r.issues = append(r.issues, iss)
		r.issuesByNum[iss.number] = iss
		return execFields(ctx, "CreateIssuePayload", iss, f.children)
	case "closeIssue":
		input, _ := f.args["input"].(map[string]any)
		idStr, _ := input["issueId"].(string)
		kind, nums := decodeGlobalID(idStr)
		if kind != "issue" || len(nums) != 2 {
			ctx.errf("Invalid issueId")
			return nil
		}
		r := store.repoByID[nums[0]]
		if r == nil {
			ctx.errf("Repository not found")
			return nil
		}
		iss := r.issuesByNum[nums[1]]
		if iss == nil {
			ctx.errf("Issue not found")
			return nil
		}
		iss.state = "closed"
		iss.jsonCache = nil
		return execFields(ctx, "CloseIssuePayload", iss, f.children)
	case "createPullRequest":
		input, _ := f.args["input"].(map[string]any)
		repoIDStr, _ := input["repositoryId"].(string)
		kind, nums := decodeGlobalID(repoIDStr)
		if kind != "repo" || len(nums) != 1 {
			ctx.errf("Invalid repositoryId")
			return nil
		}
		r := store.repoByID[nums[0]]
		if r == nil {
			ctx.errf("Repository not found")
			return nil
		}
		base, _ := input["baseRefName"].(string)
		head, _ := input["headRefName"].(string)
		title, _ := input["title"].(string)
		bodyText, _ := input["body"].(string)
		headSha, ok := r.refs["refs/heads/"+head]
		if !ok {
			ctx.errf("Head branch does not exist")
			return nil
		}
		baseSha, ok := r.refs["refs/heads/"+base]
		if !ok {
			ctx.errf("Base branch does not exist")
			return nil
		}
		pr, _ := createPullLocked(r, title, bodyText, head, base, headSha, baseSha)
		return execFields(ctx, "CreatePullRequestPayload", pr, f.children)
	case "mergePullRequest":
		input, _ := f.args["input"].(map[string]any)
		idStr, _ := input["pullRequestId"].(string)
		kind, nums := decodeGlobalID(idStr)
		if kind != "pr" || len(nums) != 2 {
			ctx.errf("Invalid pullRequestId")
			return nil
		}
		r := store.repoByID[nums[0]]
		if r == nil {
			ctx.errf("Repository not found")
			return nil
		}
		pr := r.pullsByNum[nums[1]]
		if pr == nil {
			ctx.errf("Pull request not found")
			return nil
		}
		if pr.merged || pr.state != "open" || !prMergeable(r, pr) {
			ctx.errf("Pull request cannot be merged")
			return nil
		}
		mergePullLocked(r, pr)
		return execFields(ctx, "MergePullRequestPayload", pr, f.children)
	case "addComment":
		input, _ := f.args["input"].(map[string]any)
		idStr, _ := input["subjectId"].(string)
		bodyText, _ := input["body"].(string)
		kind, nums := decodeGlobalID(idStr)
		var iss *Issue
		var r *Repo
		if kind == "issue" && len(nums) == 2 {
			r = store.repoByID[nums[0]]
			if r != nil {
				iss = r.issuesByNum[nums[1]]
			}
		}
		if iss == nil {
			ctx.errf("Subject not found")
			return nil
		}
		store.nextID++
		c := &Comment{id: store.nextID - 1, body: bodyText}
		iss.comments = append(iss.comments, c)
		iss.jsonCache = nil
		return execFields(ctx, "AddCommentPayload", map[string]any{"repo": r, "issue": iss, "comment": c}, f.children)
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, "Query")
	return nil
}

func resolveUserField(ctx *gqlCtx, parent any, f gqlField) any {
	viewer := store.getViewer()
	if viewer == "" {
		viewer = "viewer"
	}
	if login, ok := parent.(string); ok && login != "" {
		viewer = login
	}
	switch f.name {
	case "login":
		return viewer
	case "id":
		return b64id("user", 1)
	case "name":
		return viewer
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, "User")
	return nil
}

func resolveRepoField(ctx *gqlCtx, r *Repo, f gqlField) any {
	owner := r.ownerLogin()
	switch f.name {
	case "id":
		return b64id("repo", r.id)
	case "databaseId":
		return r.id
	case "name":
		return r.name
	case "nameWithOwner":
		return owner + "/" + r.name
	case "owner":
		return execFields(ctx, "User", r.ownerLogin(), f.children)
	case "defaultBranchRef":
		tip := defaultTip(r)
		if tip == "" {
			return nil
		}
		return execFields(ctx, "Ref", r.defaultBranch, f.children)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "url":
		return "https://example.invalid/" + owner + "/" + r.name
	case "createdAt":
		return fixedTS
	case "issue":
		n := argInt(f.args, "number", 0)
		iss := r.issuesByNum[n]
		if iss == nil {
			return nil
		}
		return execFields(ctx, "Issue", iss, f.children)
	case "issues":
		return issueConnection(ctx, r, f)
	case "pullRequest":
		n := argInt(f.args, "number", 0)
		pr := r.pullsByNum[n]
		if pr == nil {
			return nil
		}
		return execFields(ctx, "PullRequest", pr, f.children)
	case "pullRequests":
		return prConnection(ctx, r, f)
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, "Repository")
	return nil
}

func resolveIssueField(ctx *gqlCtx, iss *Issue, f gqlField) any {
	switch f.name {
	case "id":
		return b64id("issue", iss.repo.id, iss.number)
	case "number":
		return iss.number
	case "title":
		return iss.title
	case "body":
		return iss.body
	case "state":
		if iss.state == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "url":
		return "https://example.invalid/" + iss.repo.ownerLogin() + "/" + iss.repo.name + "/issues/" + strconv.Itoa(iss.number)
	case "createdAt":
		return fixedTS
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, "Issue")
	return nil
}

func resolvePRField(ctx *gqlCtx, pr *Pull, f gqlField) any {
	switch f.name {
	case "id":
		return b64id("pr", pr.repo.id, pr.number)
	case "number":
		return pr.number
	case "title":
		return pr.title
	case "body":
		return pr.body
	case "headRefName":
		return pr.headRef
	case "baseRefName":
		return pr.baseRef
	case "mergeable":
		if pr.merged {
			return "UNKNOWN"
		}
		if prMergeable(pr.repo, pr) {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	case "state":
		if pr.merged {
			return "MERGED"
		}
		if pr.state == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "merged":
		return pr.merged
	case "url":
		return "https://example.invalid/" + pr.repo.ownerLogin() + "/" + pr.repo.name + "/pull/" + strconv.Itoa(pr.number)
	case "createdAt":
		return fixedTS
	case "files":
		first := argInt(f.args, "first", 100)
		var nodes []any
		for i, fc := range pr.files {
			if i >= first {
				break
			}
			nodes = append(nodes, fc.filename)
		}
		return execConnection(ctx, f, "PullRequestFile", nodes, len(pr.files) > first, nil, len(pr.files))
	}
	ctx.errf("Cannot query field %q on type %q.", f.name, "PullRequest")
	return nil
}

// execConnection resolves a connection selection set (nodes / pageInfo /
// totalCount) against a list of node values.
func execConnection(ctx *gqlCtx, f gqlField, nodeType string, nodeVals []any, hasNext bool, endCursor any, total int) map[string]any {
	out := map[string]any{}
	for _, child := range f.children {
		switch child.name {
		case "nodes":
			var nodes []any
			for _, v := range nodeVals {
				nodes = append(nodes, execFields(ctx, nodeType, v, child.children))
			}
			if nodes == nil {
				nodes = []any{}
			}
			out[outKey(child)] = nodes
		case "pageInfo":
			pi := map[string]any{"hasNextPage": hasNext, "endCursor": endCursor}
			out[outKey(child)] = execFields(ctx, "Map", pi, child.children)
		case "totalCount":
			out[outKey(child)] = total
		case "edges":
			out[outKey(child)] = []any{}
		default:
			ctx.errf("Cannot query field %q on type %q.", child.name, "Connection")
			out[outKey(child)] = nil
		}
	}
	return out
}

// issueConnection resolves repository.issues with states/orderBy/after/first.
func issueConnection(ctx *gqlCtx, r *Repo, f gqlField) any {
	var list []*Issue
	// states filter
	var states []string
	if v, ok := f.args["states"]; ok {
		if arr, ok := v.([]any); ok {
			for _, s := range arr {
				if str, ok := s.(string); ok {
					states = append(states, strings.ToUpper(str))
				}
			}
		}
	}
	for _, iss := range r.issues {
		if len(states) > 0 {
			gqlState := "OPEN"
			if iss.state == "closed" {
				gqlState = "CLOSED"
			}
			match := false
			for _, s := range states {
				if s == gqlState {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, iss)
	}
	// ordering
	desc := false
	if v, ok := f.args["orderBy"]; ok {
		if m, ok := v.(map[string]any); ok {
			if d, ok := m["direction"].(string); ok && strings.ToUpper(d) == "DESC" {
				desc = true
			}
		}
	}
	if desc {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	first := argInt(f.args, "first", 100)
	start := 0
	if after := argStr(f.args, "after"); after != "" {
		if idx := cursorIndex(after); idx >= 0 {
			start = idx + 1
		}
	}
	if start > len(list) {
		start = len(list)
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	var nodes []any
	for _, iss := range list[start:end] {
		nodes = append(nodes, iss)
	}
	var endCursor any
	if end > start {
		endCursor = makeCursor(end - 1)
	}
	return execConnection(ctx, f, "Issue", nodes, end < len(list), endCursor, len(list))
}

func prConnection(ctx *gqlCtx, r *Repo, f gqlField) any {
	first := argInt(f.args, "first", 100)
	var nodes []any
	for i, pr := range r.pulls {
		if i >= first {
			break
		}
		nodes = append(nodes, pr)
	}
	return execConnection(ctx, f, "PullRequest", nodes, len(r.pulls) > first, nil, len(r.pulls))
}

func makeCursor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte("cur:" + strconv.Itoa(idx)))
}

func cursorIndex(cur string) int {
	raw, err := base64.StdEncoding.DecodeString(cur)
	if err != nil {
		return -1
	}
	s := strings.TrimPrefix(string(raw), "cur:")
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
