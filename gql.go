// GraphQL over the same store as REST: a small hand-written parser and
// executor for the fixed documents the suite sends. Global ids are the
// same opaque base64 node ids REST returns, so a node created through
// one API round-trips through the other. Unknown fields answer HTTP 200
// with an errors array naming the field, as real GraphQL does.

package main

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// global ids (opaque, stable, shared with the REST node_id shape)
// ---------------------------------------------------------------------------

func repoNodeID(repo *Repo) string { return b64("Repository:" + itoa(repo.ID)) }
func issueNodeID(repo *Repo, issue *Issue) string {
	return b64("Issue:" + itoa(repo.ID) + ":" + itoa(issue.Number))
}
func pullNodeID(repo *Repo, pull *Pull) string {
	return b64("PullRequest:" + itoa(repo.ID) + ":" + itoa(pull.Number))
}
func userNodeID(id int) string       { return b64("User:" + itoa(id)) }
func commentNodeID(id int) string    { return b64("IssueComment:" + itoa(id)) }
func itemCreatedAt(seq int64) string { return iso(epochBase + seq) }

// ---------------------------------------------------------------------------
// lexer
// ---------------------------------------------------------------------------

type gqlToken struct {
	kind  string // "name", "string", "int", "float", "punct", "eof"
	value string
}

func gqlLex(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	isNameStart := func(ch byte) bool {
		return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
	}
	isNameChar := func(ch byte) bool {
		return isNameStart(ch) || ch >= '0' && ch <= '9'
	}
	isDigit := func(ch byte) bool { return ch >= '0' && ch <= '9' }
	for i < len(source) {
		ch := source[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(source) && source[i] != '\n' {
				i++
			}
		case ch == '.' && strings.HasPrefix(source[i:], "..."):
			tokens = append(tokens, gqlToken{"punct", "..."})
			i += 3
		case strings.IndexByte("{}()[]:!=$@&|", ch) >= 0:
			tokens = append(tokens, gqlToken{"punct", string(ch)})
			i++
		case ch == '"':
			var value bytes.Buffer
			i++
			for i < len(source) && source[i] != '"' {
				c := source[i]
				if c == '\\' && i+1 < len(source) {
					esc := source[i+1]
					switch esc {
					case 'u':
						if i+5 < len(source) {
							if n, err := strconv.ParseInt(source[i+2:i+6], 16, 32); err == nil {
								value.WriteRune(rune(n))
							}
							i += 6
						} else {
							i += 2
						}
					case 'n':
						value.WriteByte('\n')
						i += 2
					case 't':
						value.WriteByte('\t')
						i += 2
					case 'r':
						value.WriteByte('\r')
						i += 2
					case 'b':
						value.WriteByte('\b')
						i += 2
					case 'f':
						value.WriteByte('\f')
						i += 2
					default:
						value.WriteByte(esc)
						i += 2
					}
				} else {
					value.WriteByte(c)
					i++
				}
			}
			i++
			tokens = append(tokens, gqlToken{"string", value.String()})
		case ch == '-' || isDigit(ch):
			start := i
			if source[i] == '-' {
				i++
			}
			for i < len(source) && isDigit(source[i]) {
				i++
			}
			isFloat := false
			if i+1 < len(source) && source[i] == '.' && isDigit(source[i+1]) {
				isFloat = true
				i++
				for i < len(source) && isDigit(source[i]) {
					i++
				}
			}
			if i < len(source) && (source[i] == 'e' || source[i] == 'E') {
				isFloat = true
				i++
				if i < len(source) && (source[i] == '+' || source[i] == '-') {
					i++
				}
				for i < len(source) && isDigit(source[i]) {
					i++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind, source[start:i]})
		case isNameStart(ch):
			start := i
			for i < len(source) && isNameChar(source[i]) {
				i++
			}
			tokens = append(tokens, gqlToken{"name", source[start:i]})
		default:
			return nil, errGQL("Unexpected character " + string(ch))
		}
	}
	tokens = append(tokens, gqlToken{"eof", ""})
	return tokens, nil
}

// ---------------------------------------------------------------------------
// parser
// ---------------------------------------------------------------------------

type gqlValue struct {
	kind    string // "var","string","int","float","bool","enum","null","list","object"
	str     string
	boolean bool
	items   []*gqlValue
	fields  map[string]*gqlValue
	varName string
}

type gqlSelection struct {
	kind          string // "field", "inlineFragment", "fragmentSpread"
	alias         string
	name          string
	args          map[string]*gqlValue
	selections    []*gqlSelection
	typeCondition string
	fragmentName  string
}

type gqlOperation struct {
	kind       string // "query" | "mutation"
	name       string
	hasName    bool
	varDefs    map[string]*gqlValue // default values (nil = none)
	selections []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string][]*gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken { t := p.tokens[p.pos]; p.pos++; return t }
func (p *gqlParser) isPunct(v string) bool {
	t := p.peek()
	return t.kind == "punct" && t.value == v
}
func (p *gqlParser) expectPunct(v string) error {
	t := p.next()
	if t.kind != "punct" || t.value != v {
		return errGQL("Expected " + v)
	}
	return nil
}
func (p *gqlParser) expectName(want string) (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", errGQL("Expected name")
	}
	if want != "" && t.value != want {
		return "", errGQL("Expected " + want)
	}
	return t.value, nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string][]*gqlSelection{}}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "name" && t.value == "fragment" {
			p.next()
			name, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, err
			}
			if _, err := p.expectName(""); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = sel
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, op)
	}
	return doc, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query", varDefs: map[string]*gqlValue{}}
	t := p.peek()
	if t.kind == "name" && (t.value == "query" || t.value == "mutation") {
		op.kind = t.value
		p.next()
		if next := p.peek(); next.kind == "name" {
			op.name = p.next().value
			op.hasName = true
		}
	}
	if p.isPunct("(") {
		p.next()
		for !p.isPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			varName, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			var def *gqlValue
			if p.isPunct("=") {
				p.next()
				def, err = p.parseValue()
				if err != nil {
					return nil, err
				}
			}
			op.varDefs[varName] = def
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sel
	return op, nil
}

func (p *gqlParser) parseType() error {
	if p.isPunct("[") {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(""); err != nil {
			return err
		}
	}
	if p.isPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*gqlSelection
	for !p.isPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.isPunct("...") {
		p.next()
		t := p.peek()
		if t.kind == "name" && t.value == "on" {
			p.next()
			cond, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "inlineFragment", typeCondition: cond, selections: sel}, nil
		}
		if t.kind == "name" {
			return &gqlSelection{kind: "fragmentSpread", fragmentName: p.next().value}, nil
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "inlineFragment", selections: sel}, nil
	}
	first, err := p.expectName("")
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{kind: "field", alias: first, name: first, args: map[string]*gqlValue{}}
	if p.isPunct(":") {
		p.next()
		name, err := p.expectName("")
		if err != nil {
			return nil, err
		}
		sel.name = name
	}
	if p.isPunct("(") {
		p.next()
		for !p.isPunct(")") {
			argName, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			sel.args[argName] = value
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	// Directives are accepted and ignored.
	for p.isPunct("@") {
		p.next()
		if _, err := p.expectName(""); err != nil {
			return nil, err
		}
		if p.isPunct("(") {
			p.next()
			for !p.isPunct(")") {
				if _, err := p.expectName(""); err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
		}
	}
	if p.isPunct("{") {
		sub, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sub
	}
	return sel, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch {
	case t.kind == "punct" && t.value == "$":
		name, err := p.expectName("")
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "var", varName: name}, nil
	case t.kind == "string":
		return &gqlValue{kind: "string", str: t.value}, nil
	case t.kind == "int":
		return &gqlValue{kind: "int", str: t.value}, nil
	case t.kind == "float":
		return &gqlValue{kind: "float", str: t.value}, nil
	case t.kind == "name":
		switch t.value {
		case "true":
			return &gqlValue{kind: "bool", boolean: true}, nil
		case "false":
			return &gqlValue{kind: "bool", boolean: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", str: t.value}, nil
		}
	case t.kind == "punct" && t.value == "[":
		v := &gqlValue{kind: "list"}
		for !p.isPunct("]") {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.items = append(v.items, item)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return v, nil
	case t.kind == "punct" && t.value == "{":
		v := &gqlValue{kind: "object", fields: map[string]*gqlValue{}}
		for !p.isPunct("}") {
			key, err := p.expectName("")
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.fields[key] = value
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, errGQL("Expected value")
}

// ---------------------------------------------------------------------------
// value resolution (variables, literals)
// ---------------------------------------------------------------------------

type gqlError string

func (e gqlError) Error() string { return string(e) }
func errGQL(msg string) error    { return gqlError(msg) }

func resolveValue(value *gqlValue, variables map[string]any, varDefs map[string]*gqlValue) any {
	switch value.kind {
	case "var":
		if v, ok := variables[value.varName]; ok {
			return v
		}
		if def := varDefs[value.varName]; def != nil {
			return resolveValue(def, variables, varDefs)
		}
		return nil
	case "string", "enum":
		return value.str
	case "int":
		n, _ := strconv.ParseInt(value.str, 10, 64)
		return float64(n)
	case "float":
		n, _ := strconv.ParseFloat(value.str, 64)
		return n
	case "bool":
		return value.boolean
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(value.items))
		for _, item := range value.items {
			out = append(out, resolveValue(item, variables, varDefs))
		}
		return out
	case "object":
		out := map[string]any{}
		for key, item := range value.fields {
			out[key] = resolveValue(item, variables, varDefs)
		}
		return out
	}
	return nil
}

func argString(args map[string]*gqlValue, name string, variables map[string]any, varDefs map[string]*gqlValue, fallback string) string {
	raw, ok := args[name]
	if !ok {
		return fallback
	}
	if v, ok := resolveValue(raw, variables, varDefs).(string); ok {
		return v
	}
	return fallback
}

func argNumber(args map[string]*gqlValue, name string, variables map[string]any, varDefs map[string]*gqlValue, fallback float64) float64 {
	raw, ok := args[name]
	if !ok {
		return fallback
	}
	if v, ok := resolveValue(raw, variables, varDefs).(float64); ok {
		return v
	}
	return fallback
}

func argInt(args map[string]*gqlValue, name string, variables map[string]any, varDefs map[string]*gqlValue, fallback int) int {
	return int(argNumber(args, name, variables, varDefs, float64(fallback)))
}

func listArg(args map[string]*gqlValue, name string, variables map[string]any, varDefs map[string]*gqlValue) []string {
	raw, ok := args[name]
	if !ok {
		return nil
	}
	value := resolveValue(raw, variables, varDefs)
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, entry := range list {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func objectArg(args map[string]*gqlValue, name string, variables map[string]any, varDefs map[string]*gqlValue) map[string]any {
	raw, ok := args[name]
	if !ok {
		return nil
	}
	if value, ok := resolveValue(raw, variables, varDefs).(map[string]any); ok {
		return value
	}
	return nil
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

// gqlSource is one executor value: a typed object plus its fields.
type gqlSource map[string]any

func src(typename string) gqlSource {
	return gqlSource{"__typename": typename}
}

func (s gqlSource) typename() string {
	if v, ok := s["__typename"].(string); ok {
		return v
	}
	return ""
}

func userSource(s *Store) gqlSource {
	return gqlSource{"__typename": "User", "login": s.viewer.Login, "id": s.viewer.ID, "name": s.viewer.Name, "email": s.viewer.Email}
}

func repoSource(repo *Repo) gqlSource {
	return gqlSource{"__typename": "Repository", "repo": repo}
}

func issueSource(repo *Repo, issue *Issue) gqlSource {
	return gqlSource{"__typename": "Issue", "repo": repo, "issue": issue}
}

func pullSource(repo *Repo, pull *Pull) gqlSource {
	return gqlSource{"__typename": "PullRequest", "repo": repo, "pull": pull}
}

type gqlEdge struct {
	node   gqlSource
	cursor string
}

type gqlPage struct {
	nodes       []gqlSource
	edges       []gqlEdge
	hasNext     bool
	hasPrev     bool
	startCursor *string
	endCursor   *string
	totalCount  int
}

func cursorOf(index int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(index)))
}

func cursorIndex(cursor string) int {
	if cursor == "" {
		return -1
	}
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return -1
	}
	text := string(raw)
	if !strings.HasPrefix(text, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

func paginateSources(items []gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) *gqlPage {
	after := argString(args, "after", variables, varDefs, "")
	first := argInt(args, "first", variables, varDefs, 30)
	start := cursorIndex(after) + 1
	if start < 0 {
		start = 0
	}
	if first < 0 {
		first = 0
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	if start > len(items) {
		start = len(items)
		end = len(items)
	}
	slice := items[start:end]
	page := &gqlPage{totalCount: len(items)}
	for i, node := range slice {
		page.nodes = append(page.nodes, node)
		page.edges = append(page.edges, gqlEdge{node: node, cursor: cursorOf(start + i)})
	}
	page.hasNext = start+len(slice) < len(items)
	page.hasPrev = start > 0
	if len(page.edges) > 0 {
		c := page.edges[len(page.edges)-1].cursor
		page.endCursor = &c
		c0 := page.edges[0].cursor
		page.startCursor = &c0
	}
	return page
}

func connectionSource(typename string, page *gqlPage) gqlSource {
	return gqlSource{"__typename": typename, "page": page}
}

func sortIssues(items []*Issue, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) []*Issue {
	states := listArg(args, "states", variables, varDefs)
	out := items
	if len(states) > 0 {
		wanted := map[string]bool{}
		for _, state := range states {
			switch state {
			case "OPEN":
				wanted["open"] = true
			case "CLOSED":
				wanted["closed"] = true
			default:
				wanted[state] = true
			}
		}
		filtered := make([]*Issue, 0, len(items))
		for _, item := range items {
			if wanted[item.State] {
				filtered = append(filtered, item)
			}
		}
		out = filtered
	}
	orderBy := objectArg(args, "orderBy", variables, varDefs)
	direction := 1
	if orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok && d == "DESC" {
			direction = -1
		}
	}
	sorted := make([]*Issue, len(out))
	copy(sorted, out)
	sortIssuesBy(sorted, direction)
	return sorted
}

func sortIssuesBy(items []*Issue, direction int) {
	// Insertion-free stable sort by (createdSeq, number) * direction.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			byCreated := (a.CreatedSeq - b.CreatedSeq) * int64(direction)
			less := byCreated < 0 || (byCreated == 0 && (int64(a.Number)-int64(b.Number))*int64(direction) < 0)
			if less {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

func sortPulls(items []*Pull, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) []*Pull {
	states := listArg(args, "states", variables, varDefs)
	out := items
	if len(states) > 0 {
		wanted := map[string]bool{}
		for _, state := range states {
			switch state {
			case "OPEN":
				wanted["open"] = true
			case "CLOSED":
				wanted["closed"] = true
			default:
				wanted[state] = true
			}
		}
		var filtered []*Pull
		for _, item := range items {
			if wanted[item.State] {
				filtered = append(filtered, item)
			}
		}
		out = filtered
	}
	orderBy := objectArg(args, "orderBy", variables, varDefs)
	direction := 1
	if orderBy != nil {
		if d, ok := orderBy["direction"].(string); ok && d == "DESC" {
			direction = -1
		}
	}
	sorted := make([]*Pull, len(out))
	copy(sorted, out)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			a, b := sorted[j-1], sorted[j]
			byCreated := (a.CreatedSeq - b.CreatedSeq) * int64(direction)
			less := byCreated < 0 || (byCreated == 0 && (int64(a.Number)-int64(b.Number))*int64(direction) < 0)
			if less {
				break
			}
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	return sorted
}

func filesOf(s *Store, pull *Pull) []ChangedFile {
	return diffPullFiles(s, pull)
}

func resolveNode(s *Store, id string) gqlSource {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return nil
	}
	kind, a := parts[0], parts[1]
	num := func(s string) int {
		n, _ := strconv.Atoi(s)
		return n
	}
	switch kind {
	case "User":
		if num(a) == s.viewer.ID {
			return userSource(s)
		}
		return nil
	case "Repository":
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, repo := range s.repos {
			if repo.ID == num(a) {
				return repoSource(repo)
			}
		}
		return nil
	case "Issue", "PullRequest":
		b := ""
		if len(parts) > 2 {
			b = parts[2]
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, repo := range s.repos {
			if repo.ID != num(a) {
				continue
			}
			if kind == "Issue" {
				if issue, ok := repo.Issues[num(b)]; ok {
					return issueSource(repo, issue)
				}
				return nil
			}
			if pull, ok := repo.Pulls[num(b)]; ok {
				return pullSource(repo, pull)
			}
			return nil
		}
		return nil
	case "IssueComment":
		commentID := num(a)
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, repo := range s.repos {
			for _, issue := range repo.Issues {
				for _, comment := range issue.Comments {
					if comment.ID == commentID {
						c := comment
						return gqlSource{"__typename": "IssueComment", "repo": repo, "issue": issue, "comment": &c}
					}
				}
			}
		}
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// introspection
// ---------------------------------------------------------------------------

var introspection = map[string]gqlSource{
	"Issue":       {"__typename": "__Type", "name": "Issue", "kind": "OBJECT", "fields": []string{"id", "number", "title", "state", "body"}, "enumValues": nil},
	"PullRequest": {"__typename": "__Type", "name": "PullRequest", "kind": "OBJECT", "fields": []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}, "enumValues": nil},
	"Repository":  {"__typename": "__Type", "name": "Repository", "kind": "OBJECT", "fields": []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}, "enumValues": nil},
	"SearchType":  {"__typename": "__Type", "name": "SearchType", "kind": "ENUM", "fields": nil, "enumValues": []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"}},
	"User":        {"__typename": "__Type", "name": "User", "kind": "OBJECT", "fields": []string{"id", "login", "name", "email"}, "enumValues": nil},
	"Query":       {"__typename": "__Type", "name": "Query", "kind": "OBJECT", "fields": []string{"viewer", "repository", "node", "__type", "__schema"}, "enumValues": nil},
	"Mutation":    {"__typename": "__Type", "name": "Mutation", "kind": "OBJECT", "fields": []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}, "enumValues": nil},
}

func introspectType(name string) gqlSource {
	if src, ok := introspection[name]; ok {
		return src
	}
	return nil
}

// ---------------------------------------------------------------------------
// field table
// ---------------------------------------------------------------------------

type gqlCtx struct {
	store *Store
}

type gqlFieldFn func(ctx *gqlCtx, source gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error)

func strField(value string) gqlFieldFn {
	return func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
		return value, nil
	}
}

func repoOf(source gqlSource) *Repo {
	if repo, ok := source["repo"].(*Repo); ok {
		return repo
	}
	return nil
}

func issueOf(source gqlSource) *Issue {
	if issue, ok := source["issue"].(*Issue); ok {
		return issue
	}
	return nil
}

func pullOf(source gqlSource) *Pull {
	if pull, ok := source["pull"].(*Pull); ok {
		return pull
	}
	return nil
}

func pageOfSource(source gqlSource) *gqlPage {
	if page, ok := source["page"].(*gqlPage); ok {
		return page
	}
	return nil
}

func gqlFields() map[string]map[string]gqlFieldFn {
	f := map[string]map[string]gqlFieldFn{}

	f["Query"] = map[string]gqlFieldFn{
		"viewer": func(ctx *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userSource(ctx.store), nil
		},
		"repository": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			owner := argString(args, "owner", variables, varDefs, "")
			name := argString(args, "name", variables, varDefs, "")
			repo := ctx.store.getRepo(owner, name)
			if repo == nil {
				return nil, nil
			}
			return repoSource(repo), nil
		},
		"node": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			id := argString(args, "id", variables, varDefs, "")
			if id == "" {
				return nil, nil
			}
			return resolveNode(ctx.store, id), nil
		},
		"__type": func(_ *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			return introspectType(argString(args, "name", variables, varDefs, "")), nil
		},
		"__schema": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return gqlSource{"__typename": "__Schema", "queryType": map[string]any{"name": "Query"}, "mutationType": map[string]any{"name": "Mutation"}}, nil
		},
	}

	f["Mutation"] = map[string]gqlFieldFn{
		"createIssue": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			input := objectArg(args, "input", variables, varDefs)
			var repoSrc gqlSource
			if input != nil {
				if id, ok := input["repositoryId"].(string); ok {
					repoSrc = resolveNode(ctx.store, id)
				}
			}
			if repoSrc == nil || repoSrc.typename() != "Repository" {
				return nil, errGQL("Could not resolve to a Repository with the provided repositoryId.")
			}
			repo := repoOf(repoSrc)
			title, _ := input["title"].(string)
			if title == "" {
				return nil, errGQL("Field `title` is required.")
			}
			body, _ := input["body"].(string)
			issue := ctx.store.createIssue(repo, title, body, nil)
			clientMutationID, _ := input["clientMutationId"].(string)
			var cmid any
			if clientMutationID != "" {
				cmid = clientMutationID
			}
			return gqlSource{"__typename": "CreateIssuePayload", "issue": issueSource(repo, issue), "clientMutationId": cmid}, nil
		},
		"closeIssue": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			input := objectArg(args, "input", variables, varDefs)
			var node gqlSource
			if input != nil {
				if id, ok := input["issueId"].(string); ok {
					node = resolveNode(ctx.store, id)
				}
			}
			if node == nil || node.typename() != "Issue" {
				return nil, errGQL("Could not resolve to an Issue with the provided issueId.")
			}
			repo := repoOf(node)
			issue := issueOf(node)
			ctx.store.closeIssue(issue)
			clientMutationID, _ := input["clientMutationId"].(string)
			var cmid any
			if clientMutationID != "" {
				cmid = clientMutationID
			}
			return gqlSource{"__typename": "CloseIssuePayload", "issue": issueSource(repo, issue), "clientMutationId": cmid}, nil
		},
		"createPullRequest": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			input := objectArg(args, "input", variables, varDefs)
			var repoSrc gqlSource
			if input != nil {
				if id, ok := input["repositoryId"].(string); ok {
					repoSrc = resolveNode(ctx.store, id)
				}
			}
			if repoSrc == nil || repoSrc.typename() != "Repository" {
				return nil, errGQL("Could not resolve to a Repository with the provided repositoryId.")
			}
			repo := repoOf(repoSrc)
			title, _ := input["title"].(string)
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			if title == "" || head == "" || base == "" {
				return nil, errGQL("Field `title`, `headRefName` and `baseRefName` are required.")
			}
			body, _ := input["body"].(string)
			pull := ctx.store.createPull(repo, title, body, head, base)
			if pull == nil {
				return nil, errGQL("Validation Failed: head or base branch does not exist.")
			}
			clientMutationID, _ := input["clientMutationId"].(string)
			var cmid any
			if clientMutationID != "" {
				cmid = clientMutationID
			}
			return gqlSource{"__typename": "CreatePullRequestPayload", "pullRequest": pullSource(repo, pull), "clientMutationId": cmid}, nil
		},
		"mergePullRequest": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			input := objectArg(args, "input", variables, varDefs)
			var node gqlSource
			if input != nil {
				if id, ok := input["pullRequestId"].(string); ok {
					node = resolveNode(ctx.store, id)
				}
			}
			if node == nil || node.typename() != "PullRequest" {
				return nil, errGQL("Could not resolve to a PullRequest with the provided pullRequestId.")
			}
			repo := repoOf(node)
			pull := pullOf(node)
			if ctx.store.mergePull(repo, pull) == "" {
				return nil, errGQL("Pull request cannot be merged.")
			}
			clientMutationID, _ := input["clientMutationId"].(string)
			var cmid any
			if clientMutationID != "" {
				cmid = clientMutationID
			}
			return gqlSource{"__typename": "MergePullRequestPayload", "pullRequest": pullSource(repo, pull), "clientMutationId": cmid}, nil
		},
		"addComment": func(ctx *gqlCtx, _ gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			input := objectArg(args, "input", variables, varDefs)
			var node gqlSource
			if input != nil {
				if id, ok := input["subjectId"].(string); ok {
					node = resolveNode(ctx.store, id)
				}
			}
			if node == nil || (node.typename() != "Issue" && node.typename() != "PullRequest") {
				return nil, errGQL("Could not resolve to an Issue with the provided subjectId.")
			}
			body, _ := input["body"].(string)
			if body == "" {
				return nil, errGQL("Field `body` is required.")
			}
			repo := repoOf(node)
			var comment Comment
			var subject gqlSource
			if node.typename() == "Issue" {
				issue := issueOf(node)
				comment = ctx.store.addComment(repo, issue, body)
				subject = issueSource(repo, issue)
			} else {
				pull := pullOf(node)
				comment = ctx.store.addPullComment(pull, body)
				subject = pullSource(repo, pull)
			}
			commentSrc := gqlSource{"__typename": "IssueComment", "repo": repo, "comment": &comment}
			clientMutationID, _ := input["clientMutationId"].(string)
			var cmid any
			if clientMutationID != "" {
				cmid = clientMutationID
			}
			return gqlSource{
				"__typename":       "AddCommentPayload",
				"commentEdge":      gqlSource{"__typename": "IssueCommentEdge", "node": commentSrc, "cursor": cursorOf(0)},
				"subject":          subject,
				"clientMutationId": cmid,
			}, nil
		},
	}

	f["User"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userNodeID(s["id"].(int)), nil
		},
		"databaseId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["id"], nil
		},
		"login": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["login"], nil
		},
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
		"email": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["email"], nil
		},
	}

	f["Repository"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoNodeID(repoOf(s)), nil
		},
		"databaseId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).ID, nil
		},
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).Name, nil
		},
		"nameWithOwner": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			return repo.Owner + "/" + repo.Name, nil
		},
		"owner": func(ctx *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userSource(ctx.store), nil
		},
		"defaultBranchRef": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return gqlSource{"__typename": "Ref", "name": repoOf(s).DefaultBranch}, nil
		},
		"viewerPermission": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return "ADMIN", nil
		},
		"hasIssuesEnabled": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return true, nil
		},
		"isPrivate": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).IsPrivate, nil
		},
		"description": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).Description, nil
		},
		"createdAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).CreatedAt, nil
		},
		"updatedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoOf(s).CreatedAt, nil
		},
		"url": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			return "https://github.com/" + repo.Owner + "/" + repo.Name, nil
		},
		"issue": func(ctx *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			number := argInt(args, "number", variables, varDefs, 0)
			ctx.store.mu.RLock()
			issue, ok := repo.Issues[number]
			ctx.store.mu.RUnlock()
			if !ok {
				return nil, nil
			}
			return issueSource(repo, issue), nil
		},
		"issues": func(ctx *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			ctx.store.mu.RLock()
			items := make([]*Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				items = append(items, issue)
			}
			ctx.store.mu.RUnlock()
			sorted := sortIssues(items, args, variables, varDefs)
			nodes := make([]gqlSource, 0, len(sorted))
			for _, issue := range sorted {
				nodes = append(nodes, issueSource(repo, issue))
			}
			return connectionSource("IssueConnection", paginateSources(nodes, args, variables, varDefs)), nil
		},
		"pullRequest": func(ctx *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			number := argInt(args, "number", variables, varDefs, 0)
			ctx.store.mu.RLock()
			pull, ok := repo.Pulls[number]
			ctx.store.mu.RUnlock()
			if !ok {
				return nil, nil
			}
			return pullSource(repo, pull), nil
		},
		"pullRequests": func(ctx *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			ctx.store.mu.RLock()
			items := make([]*Pull, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				items = append(items, pull)
			}
			ctx.store.mu.RUnlock()
			sorted := sortPulls(items, args, variables, varDefs)
			nodes := make([]gqlSource, 0, len(sorted))
			for _, pull := range sorted {
				nodes = append(nodes, pullSource(repo, pull))
			}
			return connectionSource("PullRequestConnection", paginateSources(nodes, args, variables, varDefs)), nil
		},
	}

	f["Issue"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return issueNodeID(repoOf(s), issueOf(s)), nil
		},
		"databaseId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return issueOf(s).ID, nil
		},
		"number": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return issueOf(s).Number, nil
		},
		"title": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return issueOf(s).Title, nil
		},
		"body": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return issueOf(s).Body, nil
		},
		"state": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if issueOf(s).State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		},
		"createdAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return itemCreatedAt(issueOf(s).CreatedSeq), nil
		},
		"updatedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return itemCreatedAt(issueOf(s).CreatedSeq), nil
		},
		"closedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if issueOf(s).ClosedAt == "" {
				return nil, nil
			}
			return issueOf(s).ClosedAt, nil
		},
		"author": func(ctx *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userSource(ctx.store), nil
		},
		"repository": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoSource(repoOf(s)), nil
		},
		"url": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			issue := issueOf(s)
			return "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number), nil
		},
		"comments": func(_ *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			issue := issueOf(s)
			repo := repoOf(s)
			nodes := make([]gqlSource, 0, len(issue.Comments))
			for i := range issue.Comments {
				nodes = append(nodes, gqlSource{"__typename": "IssueComment", "repo": repo, "issue": issue, "comment": &issue.Comments[i]})
			}
			return connectionSource("IssueCommentConnection", paginateSources(nodes, args, variables, varDefs)), nil
		},
		"labels": func(_ *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			issue := issueOf(s)
			nodes := make([]gqlSource, 0, len(issue.Labels))
			for _, name := range issue.Labels {
				nodes = append(nodes, gqlSource{"__typename": "Label", "name": name, "color": "ededed"})
			}
			return connectionSource("LabelConnection", paginateSources(nodes, args, variables, varDefs)), nil
		},
	}

	f["PullRequest"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullNodeID(repoOf(s), pullOf(s)), nil
		},
		"databaseId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).ID, nil
		},
		"number": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).Number, nil
		},
		"title": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).Title, nil
		},
		"body": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).Body, nil
		},
		"state": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if pullOf(s).State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		},
		"merged": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).Merged, nil
		},
		"mergeable": func(ctx *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			pull := pullOf(s)
			if pull.State != "open" {
				return "UNKNOWN", nil
			}
			return ctx.store.mergeable(pull.HeadSha, pull.BaseSha), nil
		},
		"headRefName": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).HeadRef, nil
		},
		"baseRefName": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).BaseRef, nil
		},
		"headRefOid": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).HeadSha, nil
		},
		"baseRefOid": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return pullOf(s).BaseSha, nil
		},
		"createdAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return itemCreatedAt(pullOf(s).CreatedSeq), nil
		},
		"updatedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return itemCreatedAt(pullOf(s).CreatedSeq), nil
		},
		"closedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			pull := pullOf(s)
			if pull.State == "closed" {
				return pull.MergedAt, nil
			}
			return nil, nil
		},
		"mergedAt": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if pullOf(s).MergedAt == "" {
				return nil, nil
			}
			return pullOf(s).MergedAt, nil
		},
		"author": func(ctx *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userSource(ctx.store), nil
		},
		"repository": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return repoSource(repoOf(s)), nil
		},
		"url": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			repo := repoOf(s)
			pull := pullOf(s)
			return "https://github.com/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number), nil
		},
		"files": func(ctx *gqlCtx, s gqlSource, args map[string]*gqlValue, variables map[string]any, varDefs map[string]*gqlValue) (any, error) {
			pull := pullOf(s)
			files := filesOf(ctx.store, pull)
			nodes := make([]gqlSource, 0, len(files))
			for i := range files {
				nodes = append(nodes, gqlSource{"__typename": "PullRequestFile", "file": &files[i]})
			}
			return connectionSource("FileConnection", paginateSources(nodes, args, variables, varDefs)), nil
		},
	}

	connectionFields := func(edgeTypename string) map[string]gqlFieldFn {
		return map[string]gqlFieldFn{
			"nodes": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				return pageOfSource(s).nodes, nil
			},
			"edges": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				page := pageOfSource(s)
				edges := make([]gqlSource, 0, len(page.edges))
				for _, edge := range page.edges {
					edges = append(edges, gqlSource{"__typename": edgeTypename, "node": edge.node, "cursor": edge.cursor})
				}
				return edges, nil
			},
			"pageInfo": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				page := pageOfSource(s)
				return gqlSource{"__typename": "PageInfo", "hasNextPage": page.hasNext, "hasPreviousPage": page.hasPrev, "startCursor": page.startCursor, "endCursor": page.endCursor}, nil
			},
			"totalCount": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				return pageOfSource(s).totalCount, nil
			},
		}
	}
	f["IssueConnection"] = connectionFields("IssueEdge")
	f["PullRequestConnection"] = connectionFields("PullRequestEdge")
	f["IssueCommentConnection"] = connectionFields("IssueCommentEdge")
	f["LabelConnection"] = connectionFields("LabelEdge")
	f["FileConnection"] = connectionFields("FileEdge")

	edgeFields := map[string]gqlFieldFn{
		"node": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if node, ok := s["node"].(gqlSource); ok {
				return node, nil
			}
			return nil, nil
		},
		"cursor": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			if cursor, ok := s["cursor"].(string); ok {
				return cursor, nil
			}
			return nil, nil
		},
	}
	f["IssueEdge"] = edgeFields
	f["PullRequestEdge"] = edgeFields
	f["IssueCommentEdge"] = edgeFields
	f["LabelEdge"] = edgeFields
	f["FileEdge"] = edgeFields

	f["PageInfo"] = map[string]gqlFieldFn{
		"hasNextPage": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["hasNextPage"], nil
		},
		"hasPreviousPage": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["hasPreviousPage"], nil
		},
		"startCursor": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["startCursor"], nil
		},
		"endCursor": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["endCursor"], nil
		},
	}

	f["Ref"] = map[string]gqlFieldFn{
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
	}

	f["IssueComment"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return commentNodeID(s["comment"].(*Comment).ID), nil
		},
		"databaseId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["comment"].(*Comment).ID, nil
		},
		"body": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["comment"].(*Comment).Body, nil
		},
		"author": func(ctx *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return userSource(ctx.store), nil
		},
	}

	f["Label"] = map[string]gqlFieldFn{
		"id": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return b64("Label:" + s["name"].(string)), nil
		},
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
		"color": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["color"], nil
		},
	}

	f["PullRequestFile"] = map[string]gqlFieldFn{
		"path": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["file"].(*ChangedFile).Filename, nil
		},
		"additions": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["file"].(*ChangedFile).Additions, nil
		},
		"deletions": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["file"].(*ChangedFile).Deletions, nil
		},
		"status": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["file"].(*ChangedFile).Status, nil
		},
	}

	payloadFields := func(field string) map[string]gqlFieldFn {
		return map[string]gqlFieldFn{
			field: func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				return s[field], nil
			},
			"clientMutationId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
				return s["clientMutationId"], nil
			},
		}
	}
	f["CreateIssuePayload"] = payloadFields("issue")
	f["CloseIssuePayload"] = payloadFields("issue")
	f["CreatePullRequestPayload"] = payloadFields("pullRequest")
	f["MergePullRequestPayload"] = payloadFields("pullRequest")
	f["AddCommentPayload"] = map[string]gqlFieldFn{
		"commentEdge": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["commentEdge"], nil
		},
		"subject": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["subject"], nil
		},
		"clientMutationId": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["clientMutationId"], nil
		},
	}

	f["__Type"] = map[string]gqlFieldFn{
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
		"kind": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["kind"], nil
		},
		"description": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return nil, nil
		},
		"fields": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			names, _ := s["fields"].([]string)
			if names == nil {
				return nil, nil
			}
			out := make([]gqlSource, 0, len(names))
			for _, name := range names {
				out = append(out, gqlSource{"__typename": "__Field", "name": name})
			}
			return out, nil
		},
		"enumValues": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			names, _ := s["enumValues"].([]string)
			if names == nil {
				return nil, nil
			}
			out := make([]gqlSource, 0, len(names))
			for _, name := range names {
				out = append(out, gqlSource{"__typename": "__EnumValue", "name": name})
			}
			return out, nil
		},
	}
	f["__Field"] = map[string]gqlFieldFn{
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
		"description": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return nil, nil
		},
		"args": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return []any{}, nil
		},
		"type": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return gqlSource{"__typename": "__Type", "name": "String", "kind": "SCALAR"}, nil
		},
	}
	f["__EnumValue"] = map[string]gqlFieldFn{
		"name": func(_ *gqlCtx, s gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return s["name"], nil
		},
		"description": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return nil, nil
		},
	}
	f["__Schema"] = map[string]gqlFieldFn{
		"queryType": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return gqlSource{"__typename": "__Type", "name": "Query", "kind": "OBJECT", "fields": nil, "enumValues": nil}, nil
		},
		"mutationType": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return gqlSource{"__typename": "__Type", "name": "Mutation", "kind": "OBJECT", "fields": nil, "enumValues": nil}, nil
		},
		"types": func(_ *gqlCtx, _ gqlSource, _ map[string]*gqlValue, _ map[string]any, _ map[string]*gqlValue) (any, error) {
			return []any{}, nil
		},
	}

	return f
}

var gqlFieldTable = gqlFields()

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func executeSelections(ctx *gqlCtx, source gqlSource, selections []*gqlSelection, variables map[string]any, varDefs map[string]*gqlValue, fragments map[string][]*gqlSelection) (map[string]any, error) {
	out := map[string]any{}
	for _, selection := range selections {
		switch selection.kind {
		case "inlineFragment":
			if selection.typeCondition != "" && selection.typeCondition != source.typename() {
				continue
			}
			sub, err := executeSelections(ctx, source, selection.selections, variables, varDefs, fragments)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		case "fragmentSpread":
			fragment, ok := fragments[selection.fragmentName]
			if !ok {
				return nil, errGQL(`Unknown fragment "` + selection.fragmentName + `".`)
			}
			sub, err := executeSelections(ctx, source, fragment, variables, varDefs, fragments)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		alias, name, args, sub := selection.alias, selection.name, selection.args, selection.selections
		if name == "__typename" {
			out[alias] = source.typename()
			continue
		}
		typeFields := gqlFieldTable[source.typename()]
		fn := typeFields[name]
		if fn == nil {
			return nil, errGQL(`Cannot query field "` + name + `" on type "` + source.typename() + `".`)
		}
		value, err := fn(ctx, source, args, variables, varDefs)
		if err != nil {
			return nil, err
		}
		if len(sub) == 0 {
			out[alias] = value
		} else if value == nil {
			out[alias] = nil
		} else if list, ok := value.([]gqlSource); ok {
			mapped := make([]any, 0, len(list))
			for _, entry := range list {
				subOut, err := executeSelections(ctx, entry, sub, variables, varDefs, fragments)
				if err != nil {
					return nil, err
				}
				mapped = append(mapped, subOut)
			}
			out[alias] = mapped
		} else if src, ok := value.(gqlSource); ok {
			subOut, err := executeSelections(ctx, src, sub, variables, varDefs, fragments)
			if err != nil {
				return nil, err
			}
			out[alias] = subOut
		} else if list, ok := value.([]any); ok {
			mapped := make([]any, 0, len(list))
			for _, entry := range list {
				if src, ok := entry.(gqlSource); ok {
					subOut, err := executeSelections(ctx, src, sub, variables, varDefs, fragments)
					if err != nil {
						return nil, err
					}
					mapped = append(mapped, subOut)
				} else {
					mapped = append(mapped, entry)
				}
			}
			out[alias] = mapped
		} else {
			return nil, errGQL(`Field "` + name + `" on type "` + source.typename() + `" must not have a selection since type has no subfields.`)
		}
	}
	return out, nil
}

func runOperation(body any, ctx *gqlCtx) map[string]any {
	record, ok := body.(map[string]any)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "GraphQL request body must be an object with a query field."}}}
	}
	query, ok := record["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Must provide query string."}}}
	}
	variables := map[string]any{}
	if v, ok := record["variables"].(map[string]any); ok {
		variables = v
	}
	operationName, _ := record["operationName"].(string)
	hasOperationName := false
	if _, ok := record["operationName"].(string); ok {
		hasOperationName = true
	}
	tokens, err := gqlLex(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	parser := &gqlParser{tokens: tokens}
	document, err := parser.parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	var operation *gqlOperation
	if hasOperationName {
		for _, op := range document.operations {
			if op.hasName && op.name == operationName {
				operation = op
				break
			}
		}
	} else if len(document.operations) == 1 {
		operation = document.operations[0]
	} else {
		for _, op := range document.operations {
			if !op.hasName {
				operation = op
				break
			}
		}
	}
	if operation == nil {
		msg := "Must provide an operation name if there are multiple operations."
		if hasOperationName {
			msg = `Unknown operation named "` + operationName + `".`
		}
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": msg}}}
	}
	kind := "Query"
	if operation.kind == "mutation" {
		kind = "Mutation"
	}
	root := gqlSource{"__typename": kind}
	data, err := executeSelections(ctx, root, operation.selections, variables, operation.varDefs, document.fragments)
	if err != nil {
		if _, ok := err.(gqlError); ok {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
		}
		// A non-GraphQL error is the handler's to surface as a 500.
		panic(err)
	}
	return map[string]any{"data": data}
}

// ---------------------------------------------------------------------------
// handler
// ---------------------------------------------------------------------------

func graphqlHandler(w *Responder, req *Request, s *Store) {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return
	}
	if req.JSONBad {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	ctx := &gqlCtx{store: s}
	if list, ok := req.JSON.([]any); ok {
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, runOperation(entry, ctx))
		}
		w.writeObj(req, 200, out, nil)
		return
	}
	w.writeObj(req, 200, runOperation(req.JSON, ctx), nil)
}
