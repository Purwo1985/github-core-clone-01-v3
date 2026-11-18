// MCP (Model Context Protocol) over Streamable HTTP: JSON-RPC 2.0 on
// POST /mcp. Both the modern `server/discover` era (2026-07-28) and the
// legacy `initialize` handshake are served, and the tools act on the same
// store as REST and GraphQL, so a tool call is visible through every API.

package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
)

func marshalJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return raw
}

const mcpServerName = "forge-mcp"
const mcpServerVersion = "1.0.0"

var mcpProtocolVersions = []string{"2026-07-28", "2025-06-18", "2024-11-05"}
var mcpSessionCounter int64

// ---------------------------------------------------------------------------
// tool definitions: the list response is a frozen constant, so two
// tools/list calls are byte-identical.
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	name        string
	description string
	properties  []mcpToolProp
	required    []string
}

type mcpToolProp struct {
	name        string
	typ         string
	description string
}

var mcpTools = []mcpToolDef{
	{
		name:        "create_repo",
		description: "Create a new repository for the authenticated user.",
		properties: []mcpToolProp{
			{"name", "string", "The name of the repository."},
			{"description", "string", "A short description of the repository."},
			{"private", "boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Get a repository by owner and name.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
			{"title", "string", "The title of the issue."},
			{"body", "string", "The body of the issue."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List issues in a repository, optionally filtered by state.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
			{"state", "string", "Filter by state: open, closed or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "get_issue",
		description: "Get a single issue by number.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
			{"issue_number", "integer", "The issue number."},
		},
		required: []string{"owner", "repo", "issue_number"},
	},
	{
		name:        "create_pull_request",
		description: "Create a pull request from a head branch onto a base branch.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
			{"title", "string", "The title of the pull request."},
			{"head", "string", "The head branch."},
			{"base", "string", "The base branch."},
			{"body", "string", "The body of the pull request."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request.",
		properties: []mcpToolProp{
			{"owner", "string", "The owner of the repository."},
			{"repo", "string", "The name of the repository."},
			{"full_name", "string", "The full name of the repository, owner/repo."},
			{"pull_number", "integer", "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpToolJSON(def mcpToolDef) map[string]any {
	properties := map[string]any{}
	for _, prop := range def.properties {
		properties[prop.name] = map[string]any{"type": prop.typ, "description": prop.description}
	}
	return map[string]any{
		"name":        def.name,
		"description": def.description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   def.required,
		},
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mcpResult(w *Responder, req *Request, id any, result any, extra map[string]string) {
	w.writeObj(req, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, extra)
}

func mcpError(w *Responder, req *Request, id any, code int, message string, httpStatus int) {
	w.writeObj(req, httpStatus, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}, nil)
}

func mcpToolText(w *Responder, req *Request, id any, payload any) {
	text := ""
	if s, ok := payload.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(payload))
	}
	mcpResult(w, req, id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": payload,
	}, nil)
}

func mcpToolError(w *Responder, req *Request, id any, message string) {
	mcpResult(w, req, id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "Error: " + message}},
		"isError": true,
	}, nil)
}

func mcpStrArg(args map[string]any, name string) string {
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}

func mcpNumArg(args map[string]any, name string) int {
	switch v := args[name].(type) {
	case float64:
		return int(v)
	case string:
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return 0
}

// resolveRepo resolves a repository from owner+repo, or from a full_name
// fallback.
func mcpResolveRepo(s *Store, args map[string]any) *Repo {
	owner := mcpStrArg(args, "owner")
	name := mcpStrArg(args, "repo")
	if owner != "" && name != "" {
		if repo := s.getRepo(owner, name); repo != nil {
			return repo
		}
	}
	fullName := mcpStrArg(args, "full_name")
	if slash := strings.Index(fullName, "/"); slash > 0 {
		return s.getRepo(fullName[:slash], fullName[slash+1:])
	}
	return nil
}

func mcpRepoSummary(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          map[string]any{"login": repo.Owner},
		"private":        repo.IsPrivate,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func mcpIssueSummary(repo *Repo, issue *Issue) map[string]any {
	return map[string]any{
		"id":       issue.ID,
		"number":   issue.Number,
		"title":    issue.Title,
		"body":     issue.Body,
		"state":    issue.State,
		"labels":   issue.Labels,
		"comments": len(issue.Comments),
		"url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
	}
}

func mcpPullSummary(repo *Repo, pull *Pull) map[string]any {
	var mergeSha any
	if pull.MergeCommit != "" {
		mergeSha = pull.MergeCommit
	}
	return map[string]any{
		"id":               pull.ID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"head":             pull.HeadRef,
		"base":             pull.BaseRef,
		"merge_commit_sha": mergeSha,
		"url":              "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number),
	}
}

// ---------------------------------------------------------------------------
// tool execution
// ---------------------------------------------------------------------------

func mcpCallTool(w *Responder, req *Request, s *Store, id any, params any) {
	record, ok := params.(map[string]any)
	if !ok {
		mcpError(w, req, id, -32602, "Invalid params: expected an object with name and arguments.", 200)
		return
	}
	rawName, _ := record["name"].(string)
	args, _ := record["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Tools may carry a server prefix; match by suffix, case-insensitively.
	lower := strings.ToLower(rawName)
	var tool *mcpToolDef
	for i := range mcpTools {
		candidate := strings.ToLower(mcpTools[i].name)
		if lower == candidate || strings.HasSuffix(lower, "_"+candidate) || strings.HasSuffix(lower, candidate) {
			tool = &mcpTools[i]
			break
		}
	}
	if tool == nil {
		mcpError(w, req, id, -32601, "Unknown tool: "+rawName, 200)
		return
	}
	hasFullName := mcpStrArg(args, "full_name") != ""
	var missing []string
	for _, field := range tool.required {
		value := args[field]
		if value == nil || value == "" {
			if (field == "owner" || field == "repo") && hasFullName {
				continue
			}
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		mcpToolError(w, req, id, "Missing required argument(s): "+strings.Join(missing, ", "))
		return
	}
	switch tool.name {
	case "create_repo":
		name := mcpStrArg(args, "name")
		description := mcpStrArg(args, "description")
		isPrivate := args["private"] == true
		if existing := s.getRepo(s.viewer.Login, name); existing != nil {
			mcpToolError(w, req, id, "Repository "+s.viewer.Login+"/"+name+" already exists.")
			return
		}
		repo := s.createRepo(s.viewer.Login, name, isPrivate, description)
		mcpToolText(w, req, id, mcpRepoSummary(repo))
		return
	case "get_repo":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		mcpToolText(w, req, id, mcpRepoSummary(repo))
		return
	case "create_issue":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		issue := s.createIssue(repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"), nil)
		mcpToolText(w, req, id, mcpIssueSummary(repo, issue))
		return
	case "list_issues":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		state := mcpStrArg(args, "state")
		if state == "" {
			state = "open"
		}
		s.mu.RLock()
		items := make([]*Issue, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				items = append(items, issue)
			}
		}
		s.mu.RUnlock()
		for i := 1; i < len(items); i++ {
			for j := i; j > 0 && items[j].CreatedSeq < items[j-1].CreatedSeq; j-- {
				items[j-1], items[j] = items[j], items[j-1]
			}
		}
		summaries := make([]any, 0, len(items))
		for _, issue := range items {
			summaries = append(summaries, mcpIssueSummary(repo, issue))
		}
		mcpToolText(w, req, id, map[string]any{"issues": summaries})
		return
	case "get_issue":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		s.mu.RLock()
		issue, ok := repo.Issues[mcpNumArg(args, "issue_number")]
		s.mu.RUnlock()
		if !ok {
			mcpToolError(w, req, id, "Issue not found.")
			return
		}
		mcpToolText(w, req, id, mcpIssueSummary(repo, issue))
		return
	case "create_pull_request":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		pull := s.createPull(repo, mcpStrArg(args, "title"), mcpStrArg(args, "body"), mcpStrArg(args, "head"), mcpStrArg(args, "base"))
		if pull == nil {
			mcpToolError(w, req, id, "Could not create pull request: head or base branch does not exist, or a pull request already exists for this head.")
			return
		}
		mcpToolText(w, req, id, mcpPullSummary(repo, pull))
		return
	case "merge_pull_request":
		repo := mcpResolveRepo(s, args)
		if repo == nil {
			mcpToolError(w, req, id, "Repository not found.")
			return
		}
		s.mu.RLock()
		pull, ok := repo.Pulls[mcpNumArg(args, "pull_number")]
		s.mu.RUnlock()
		if !ok {
			mcpToolError(w, req, id, "Pull request not found.")
			return
		}
		mergeSha := s.mergePull(repo, pull)
		if mergeSha == "" {
			mcpToolError(w, req, id, "Pull request cannot be merged.")
			return
		}
		summary := mcpPullSummary(repo, pull)
		summary["merged"] = true
		summary["sha"] = mergeSha
		mcpToolText(w, req, id, summary)
		return
	default:
		mcpError(w, req, id, -32601, "Unknown tool: "+rawName, 200)
	}
}

// ---------------------------------------------------------------------------
// handler
// ---------------------------------------------------------------------------

func mcpHandler(w *Responder, req *Request, s *Store) {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return
	}
	if req.JSONBad || req.JSON == nil {
		mcpError(w, req, nil, -32700, "Parse error: request body is not valid JSON.", 400)
		return
	}
	body, ok := req.JSON.(map[string]any)
	if !ok {
		mcpError(w, req, nil, -32600, "Invalid Request: expected a JSON-RPC object.", 200)
		return
	}
	method, _ := body["method"].(string)
	id, hasID := body["id"]
	if method == "" {
		mcpError(w, req, nil, -32600, "Invalid Request: missing method.", 200)
		return
	}
	// Notifications (no id) are acknowledged with an empty 202.
	if !hasID {
		w.writeEmpty(req, 202, nil)
		return
	}
	serverInfo := map[string]any{"name": mcpServerName, "version": mcpServerVersion}
	switch method {
	case "server/discover":
		mcpResult(w, req, id, map[string]any{
			"supportedVersions": mcpProtocolVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}, nil)
		return
	case "initialize":
		n := atomic.AddInt64(&mcpSessionCounter, 1)
		mcpResult(w, req, id, map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo,
			"_meta":           map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}, map[string]string{"Mcp-Session-Id": "forge-" + itoa(int(n))})
		return
	case "ping":
		mcpResult(w, req, id, map[string]any{}, nil)
		return
	case "tools/list":
		tools := make([]any, 0, len(mcpTools))
		for _, def := range mcpTools {
			tools = append(tools, mcpToolJSON(def))
		}
		mcpResult(w, req, id, map[string]any{"tools": tools}, nil)
		return
	case "tools/call":
		mcpCallTool(w, req, s, id, body["params"])
		return
	default:
		mcpError(w, req, id, -32601, "Method not found: "+method, 200)
	}
}
