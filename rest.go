// REST v3: repositories, branches, commits, contents, labels, issues and
// pull requests. Serializers build GitHub-shaped JSON; the hot reads
// (one repository, one issue) are cached as strings on the record and
// invalidated by the mutations that touch them.

package main

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func b64(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// authed reports whether the request carries the trial's token (or any
// token in dev mode, when the store has none).
func authed(req *Request, s *Store) bool {
	if s.token == "" {
		return true
	}
	return req.Token != "" && req.Token == s.token
}

func requireAuth(w *Responder, req *Request, s *Store) bool {
	if !authed(req, s) {
		w.writeErr(req, 401, "Requires authentication")
		return false
	}
	return true
}

// bodyObj returns the JSON object body, or nil when the body was not a
// JSON object.
func bodyObj(req *Request) map[string]any {
	if req.JSONBad {
		return nil
	}
	if req.JSON == nil {
		return map[string]any{}
	}
	if obj, ok := req.JSON.(map[string]any); ok {
		return obj
	}
	return nil
}

// jsonOk answers 400 when the body was malformed JSON.
func jsonOk(w *Responder, req *Request) bool {
	if req.JSONBad {
		w.writeErr(req, 400, "Invalid JSON")
		return false
	}
	return true
}

func bodyStr(body map[string]any, key, fallback string) string {
	if v, ok := body[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return fallback
}

func bodyBool(body map[string]any, key string, fallback bool) bool {
	if v, ok := body[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return fallback
}

func queryInt(req *Request, name string, fallback int) int {
	raw := req.Query.Get(name)
	if raw == "" {
		return fallback
	}
	if v, err := strconv.Atoi(raw); err == nil {
		return v
	}
	return fallback
}

// pageOf parses pagination: per_page (default 30, max 100) and page
// (default 1).
func pageOf(req *Request) (perPage, page int) {
	perPage = queryInt(req, "per_page", 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = queryInt(req, "page", 1)
	if page < 1 {
		page = 1
	}
	return perPage, page
}

// linkHeader builds the Link header for a paginated list, with absolute
// same-origin URLs.
func linkHeader(req *Request, total, perPage, page int) string {
	host := req.Headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	base := "http://" + host + req.Path
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	urlFor := func(p int) string {
		q := url.Values{}
		for k, vs := range req.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("per_page", itoa(perPage))
		q.Set("page", itoa(p))
		return base + "?" + q.Encode()
	}
	var parts []string
	if page < lastPage {
		parts = append(parts, "<"+urlFor(page+1)+`>; rel="next"`)
	}
	if page > 1 {
		parts = append(parts, "<"+urlFor(page-1)+`>; rel="prev"`)
	}
	parts = append(parts, "<"+urlFor(1)+`>; rel="first"`)
	parts = append(parts, "<"+urlFor(lastPage)+`>; rel="last"`)
	return strings.Join(parts, ", ")
}

func paginateLen(total, perPage, page int) (start, end int) {
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = start + perPage
	if end > total {
		end = total
	}
	return start, end
}

// ---------------------------------------------------------------------------
// serializers
// ---------------------------------------------------------------------------

// Precomputed viewer JSON (the viewer never changes within a trial).
var userRefJSON string
var userJSON string

func initViewerJSON(viewer Identity) {
	var ref bytes.Buffer
	ref.WriteString(`{"login":`)
	writeJSONString(&ref, viewer.Login)
	ref.WriteString(`,"id":`)
	ref.WriteString(itoa(viewer.ID))
	ref.WriteString(`,"node_id":`)
	writeJSONString(&ref, b64("User:"+itoa(viewer.ID)))
	ref.WriteString(`,"avatar_url":`)
	writeJSONString(&ref, "https://avatars.githubusercontent.com/u/"+itoa(viewer.ID)+"?v=4")
	ref.WriteString(`,"gravatar_id":"","url":`)
	writeJSONString(&ref, "/api/v3/users/"+viewer.Login)
	ref.WriteString(`,"html_url":`)
	writeJSONString(&ref, "https://github.com/"+viewer.Login)
	ref.WriteString(`,"type":"User","site_admin":false}`)
	userRefJSON = ref.String()

	var full bytes.Buffer
	full.WriteString(`{"login":`)
	writeJSONString(&full, viewer.Login)
	full.WriteString(`,"id":`)
	full.WriteString(itoa(viewer.ID))
	full.WriteString(`,"node_id":`)
	writeJSONString(&full, b64("User:"+itoa(viewer.ID)))
	full.WriteString(`,"avatar_url":`)
	writeJSONString(&full, "https://avatars.githubusercontent.com/u/"+itoa(viewer.ID)+"?v=4")
	full.WriteString(`,"gravatar_id":"","url":`)
	writeJSONString(&full, "/api/v3/users/"+viewer.Login)
	full.WriteString(`,"html_url":`)
	writeJSONString(&full, "https://github.com/"+viewer.Login)
	full.WriteString(`,"type":"User","site_admin":false,"name":`)
	writeJSONString(&full, viewer.Name)
	full.WriteString(`,"email":`)
	writeJSONString(&full, viewer.Email)
	full.WriteString(`}`)
	userJSON = full.String()
}

func labelJSONBytes(label *Label) string {
	var buf bytes.Buffer
	buf.WriteString(`{"id":0,"node_id":`)
	writeJSONString(&buf, b64("Label:"+label.Name))
	buf.WriteString(`,"url":`)
	writeJSONString(&buf, "/api/v3/labels/"+url.PathEscape(label.Name))
	buf.WriteString(`,"name":`)
	writeJSONString(&buf, label.Name)
	buf.WriteString(`,"color":`)
	writeJSONString(&buf, label.Color)
	buf.WriteString(`,"default":false,"description":""}`)
	return buf.String()
}

// repoJSON renders a repository; cached on the record.
func repoJSON(s *Store, repo *Repo) string {
	s.mu.RLock()
	if repo.JSONCache != "" {
		cached := repo.JSONCache
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	openCount := 0
	s.mu.RLock()
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			openCount++
		}
	}
	s.mu.RUnlock()

	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.WriteString(itoa(repo.ID))
	buf.WriteString(`,"node_id":`)
	writeJSONString(&buf, b64("Repository:"+itoa(repo.ID)))
	buf.WriteString(`,"name":`)
	writeJSONString(&buf, repo.Name)
	buf.WriteString(`,"full_name":`)
	writeJSONString(&buf, repo.Owner+"/"+repo.Name)
	buf.WriteString(`,"owner":`)
	buf.WriteString(userRefJSON)
	buf.WriteString(`,"private":`)
	buf.WriteString(boolStr(repo.IsPrivate))
	buf.WriteString(`,"html_url":`)
	writeJSONString(&buf, "https://github.com/"+repo.Owner+"/"+repo.Name)
	buf.WriteString(`,"description":`)
	writeJSONString(&buf, repo.Description)
	buf.WriteString(`,"fork":false,"url":`)
	writeJSONString(&buf, "/api/v3/repos/"+repo.Owner+"/"+repo.Name)
	buf.WriteString(`,"created_at":`)
	writeJSONString(&buf, repo.CreatedAt)
	buf.WriteString(`,"updated_at":`)
	writeJSONString(&buf, repo.CreatedAt)
	buf.WriteString(`,"pushed_at":`)
	writeJSONString(&buf, repo.CreatedAt)
	buf.WriteString(`,"git_url":`)
	writeJSONString(&buf, "git://github.com/"+repo.Owner+"/"+repo.Name+".git")
	buf.WriteString(`,"ssh_url":`)
	writeJSONString(&buf, "git@github.com:"+repo.Owner+"/"+repo.Name+".git")
	buf.WriteString(`,"clone_url":`)
	writeJSONString(&buf, "https://github.com/"+repo.Owner+"/"+repo.Name+".git")
	buf.WriteString(`,"homepage":null,"size":0,"stargazers_count":0,"watchers_count":0,"language":null`)
	buf.WriteString(`,"has_issues":true,"has_projects":true,"has_downloads":true,"has_wiki":true,"has_pages":false,"has_discussions":false`)
	buf.WriteString(`,"forks_count":0,"mirror_url":null,"archived":false,"disabled":false,"open_issues_count":`)
	buf.WriteString(itoa(openCount))
	buf.WriteString(`,"license":null,"allow_forking":true,"is_template":false,"web_commit_signoff_required":false,"topics":[]`)
	buf.WriteString(`,"visibility":`)
	writeJSONString(&buf, map[bool]string{true: "private", false: "public"}[repo.IsPrivate])
	buf.WriteString(`,"forks":0,"open_issues":0,"watchers":0,"default_branch":`)
	writeJSONString(&buf, repo.DefaultBranch)
	buf.WriteString(`,"permissions":{"admin":true,"maintain":true,"push":true,"triage":true,"pull":true}}`)
	text := buf.String()

	s.mu.Lock()
	repo.JSONCache = text
	s.mu.Unlock()
	return text
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// issueJSON renders an issue; cached on the record.
func issueJSON(s *Store, repo *Repo, issue *Issue) string {
	s.mu.RLock()
	if issue.JSONCache != "" {
		cached := issue.JSONCache
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.WriteString(itoa(issue.ID))
	buf.WriteString(`,"node_id":`)
	writeJSONString(&buf, b64("Issue:"+itoa(repo.ID)+":"+itoa(issue.Number)))
	buf.WriteString(`,"url":`)
	writeJSONString(&buf, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number))
	buf.WriteString(`,"repository_url":`)
	writeJSONString(&buf, "/api/v3/repos/"+repo.Owner+"/"+repo.Name)
	buf.WriteString(`,"html_url":`)
	writeJSONString(&buf, "https://github.com/"+repo.Owner+"/"+repo.Name+"/issues/"+itoa(issue.Number))
	buf.WriteString(`,"number":`)
	buf.WriteString(itoa(issue.Number))
	buf.WriteString(`,"title":`)
	writeJSONString(&buf, issue.Title)
	buf.WriteString(`,"body":`)
	writeJSONString(&buf, issue.Body)
	buf.WriteString(`,"user":`)
	buf.WriteString(userRefJSON)
	buf.WriteString(`,"labels":[`)
	s.mu.RLock()
	labels := issue.Labels
	comments := len(issue.Comments)
	s.mu.RUnlock()
	for i, name := range labels {
		if i > 0 {
			buf.WriteByte(',')
		}
		s.mu.RLock()
		stored, ok := repo.Labels[name]
		s.mu.RUnlock()
		if ok {
			buf.WriteString(labelJSONBytes(stored))
		} else {
			buf.WriteString(labelJSONBytes(&Label{Name: name, Color: "ededed"}))
		}
	}
	buf.WriteString(`],"state":`)
	writeJSONString(&buf, issue.State)
	buf.WriteString(`,"locked":false,"assignee":null,"assignees":[],"milestone":null,"comments":`)
	buf.WriteString(itoa(comments))
	buf.WriteString(`,"created_at":`)
	writeJSONString(&buf, iso(epochBase+issue.CreatedSeq))
	buf.WriteString(`,"updated_at":`)
	writeJSONString(&buf, iso(epochBase+issue.CreatedSeq))
	buf.WriteString(`,"closed_at":`)
	if issue.ClosedAt == "" {
		buf.WriteString("null")
	} else {
		writeJSONString(&buf, issue.ClosedAt)
	}
	buf.WriteString(`,"author_association":"OWNER","active_lock_reason":null,"draft":false}`)
	text := buf.String()

	s.mu.Lock()
	issue.JSONCache = text
	s.mu.Unlock()
	return text
}

// pullJSON renders a pull request.
func pullJSON(s *Store, repo *Repo, pull *Pull) string {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	buf.WriteString(itoa(pull.ID))
	buf.WriteString(`,"node_id":`)
	writeJSONString(&buf, b64("PullRequest:"+itoa(repo.ID)+":"+itoa(pull.Number)))
	buf.WriteString(`,"url":`)
	writeJSONString(&buf, "/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/pulls/"+itoa(pull.Number))
	buf.WriteString(`,"html_url":`)
	writeJSONString(&buf, "https://github.com/"+repo.Owner+"/"+repo.Name+"/pulls/"+itoa(pull.Number))
	buf.WriteString(`,"number":`)
	buf.WriteString(itoa(pull.Number))
	buf.WriteString(`,"state":`)
	writeJSONString(&buf, pull.State)
	buf.WriteString(`,"title":`)
	writeJSONString(&buf, pull.Title)
	buf.WriteString(`,"body":`)
	writeJSONString(&buf, pull.Body)
	buf.WriteString(`,"user":`)
	buf.WriteString(userRefJSON)
	buf.WriteString(`,"labels":[],"milestone":null,"locked":false,"created_at":`)
	writeJSONString(&buf, iso(epochBase+pull.CreatedSeq))
	buf.WriteString(`,"updated_at":`)
	writeJSONString(&buf, iso(epochBase+pull.CreatedSeq))
	buf.WriteString(`,"closed_at":`)
	if pull.State == "closed" {
		writeJSONString(&buf, pull.MergedAt)
	} else {
		buf.WriteString("null")
	}
	buf.WriteString(`,"merged_at":`)
	if pull.MergedAt == "" {
		buf.WriteString("null")
	} else {
		writeJSONString(&buf, pull.MergedAt)
	}
	buf.WriteString(`,"merge_commit_sha":`)
	if pull.MergeCommit == "" {
		buf.WriteString("null")
	} else {
		writeJSONString(&buf, pull.MergeCommit)
	}
	buf.WriteString(`,"draft":false`)
	refPair := func(ref, sha string) {
		buf.WriteString(`{"label":`)
		writeJSONString(&buf, repo.Owner+":"+ref)
		buf.WriteString(`,"ref":`)
		writeJSONString(&buf, ref)
		buf.WriteString(`,"sha":`)
		writeJSONString(&buf, sha)
		buf.WriteString(`,"user":`)
		buf.WriteString(userRefJSON)
		buf.WriteString(`,"repo":`)
		buf.WriteString(repoJSON(s, repo))
		buf.WriteString(`}`)
	}
	buf.WriteString(`,"head":`)
	refPair(pull.HeadRef, pull.HeadSha)
	buf.WriteString(`,"base":`)
	refPair(pull.BaseRef, pull.BaseSha)
	buf.WriteString(`,"merged":`)
	buf.WriteString(boolStr(pull.Merged))
	buf.WriteString(`,"mergeable":`)
	if pull.State == "open" {
		buf.WriteString(boolStr(s.mergeable(pull.HeadSha, pull.BaseSha) == "MERGEABLE"))
	} else {
		buf.WriteString("null")
	}
	buf.WriteString(`,"mergeable_state":`)
	if pull.State == "open" {
		buf.WriteString(`"clean"`)
	} else {
		buf.WriteString(`"unknown"`)
	}
	buf.WriteString(`,"merged_by":null,"comments":0,"review_comments":0,"commits":1,"additions":0,"deletions":0,"changed_files":0}`)
	return buf.String()
}

// commentJSON renders an issue comment (REST list shape: user is a
// {login, id} pair).
func commentJSON(s *Store, repo *Repo, issue *Issue, comment Comment) map[string]any {
	stamp := iso(epochBase + issue.CreatedSeq)
	return map[string]any{
		"id":         comment.ID,
		"node_id":    b64("IssueComment:" + itoa(comment.ID)),
		"url":        "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number) + "/comments/" + itoa(comment.ID),
		"html_url":   "https://github.com/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number) + "#issuecomment-" + itoa(comment.ID),
		"body":       comment.Body,
		"user":       map[string]any{"login": s.viewer.Login, "id": s.viewer.ID},
		"created_at": stamp,
		"updated_at": stamp,
	}
}

// ---------------------------------------------------------------------------
// repo lookup
// ---------------------------------------------------------------------------

func lookupRepo(w *Responder, req *Request, s *Store) *Repo {
	owner := req.Segments[3]
	name := req.Segments[4]
	repo := s.getRepo(owner, name)
	if repo == nil {
		w.writeErr(req, 404, "Not Found")
		return nil
	}
	return repo
}

// ---------------------------------------------------------------------------
// API root, meta, user
// ---------------------------------------------------------------------------

func apiRoot(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	w.writeObj(req, 200, map[string]any{
		"current_user_url": "/api/v3/user",
		"rate_limit_url":   "/api/v3/rate_limit",
	}, map[string]string{"x-oauth-scopes": "repo, read:org"})
}

func apiMeta(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	w.writeObj(req, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true}, nil)
}

func getUser(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	w.writeJSON(req, 200, userJSON, nil)
}

// ---------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func createRepo(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	name := bodyStr(body, "name", "")
	if name == "" || !repoNameRe.MatchString(name) {
		w.writeErr(req, 422, "Validation Failed: name is invalid")
		return
	}
	if s.getRepo(s.viewer.Login, name) != nil {
		w.writeErr(req, 422, "Repository already exists")
		return
	}
	repo := s.createRepo(s.viewer.Login, name, bodyBool(body, "private", false), bodyStr(body, "description", ""))
	w.writeJSON(req, 201, repoJSON(s, repo), nil)
}

func listUserRepos(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	perPage, page := pageOf(req)
	s.mu.RLock()
	var all []*Repo
	for _, repo := range s.repos {
		if repo.Owner == s.viewer.Login {
			all = append(all, repo)
		}
	}
	s.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	start, end := paginateLen(len(all), perPage, page)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			buf.WriteByte(',')
		}
		buf.WriteString(repoJSON(s, all[i]))
	}
	buf.WriteByte(']')
	var extra map[string]string
	if page*perPage < len(all) || page > 1 {
		extra = map[string]string{"link": linkHeader(req, len(all), perPage, page)}
	}
	w.writeJSON(req, 200, buf.String(), extra)
}

func getRepo(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	w.writeJSON(req, 200, repoJSON(s, repo), nil)
}

func patchRepo(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	if desc, ok := body["description"].(string); ok {
		s.mu.Lock()
		repo.Description = desc
		repo.JSONCache = ""
		s.mu.Unlock()
	}
	w.writeJSON(req, 200, repoJSON(s, repo), nil)
}

func deleteRepo(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	s.deleteRepo(repo)
	w.writeEmpty(req, 204, nil)
}

// ---------------------------------------------------------------------------
// branches, commits, git refs
// ---------------------------------------------------------------------------

func listBranches(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	s.mu.RLock()
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, ref)
		}
	}
	s.mu.RUnlock()
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, ref := range names {
		name := strings.TrimPrefix(ref, "refs/heads/")
		s.mu.RLock()
		sha := repo.Refs[ref]
		s.mu.RUnlock()
		out = append(out, map[string]any{
			"name": name,
			"commit": map[string]any{
				"sha": sha,
				"url": "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + sha,
			},
			"protected": false,
		})
	}
	w.writeObj(req, 200, out, nil)
}

func getBranch(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	branch := req.Segments[6]
	s.mu.RLock()
	sha, ok := repo.Refs["refs/heads/"+branch]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Branch not found")
		return
	}
	w.writeObj(req, 200, map[string]any{
		"name": branch,
		"commit": map[string]any{
			"sha": sha,
			"url": "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + sha,
		},
		"protected": false,
	}, nil)
}

func listCommits(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	shaParam := req.Query.Get("sha")
	var tip string
	var hasTip bool
	s.mu.RLock()
	if shaParam != "" {
		if v, ok := repo.Refs["refs/heads/"+shaParam]; ok {
			tip, hasTip = v, true
		} else if v, ok := repo.Refs[shaParam]; ok {
			tip, hasTip = v, true
		} else if isOID(shaParam) {
			tip, hasTip = shaParam, true
		}
	} else {
		tip, hasTip = repo.Refs[s.headTarget(repo)]
	}
	s.mu.RUnlock()
	out := make([]map[string]any, 0)
	if hasTip {
		seen := map[string]bool{}
		queue := []string{tip}
		head := 0
		for head < len(queue) && len(out) < 100 {
			oid := queue[head]
			head++
			if seen[oid] {
				continue
			}
			seen[oid] = true
			object, ok := s.getObject(oid)
			if !ok || object.Type != objCommit {
				continue
			}
			view := parseCommit(object.Data)
			authorLine := view.Author
			date := repo.CreatedAt
			ident := authorLine
			// The timestamp is " <epoch> +0000" at the end of the line.
			if strings.HasSuffix(authorLine, " +0000") {
				trimmed := authorLine[:len(authorLine)-len(" +0000")]
				if idx := strings.LastIndex(trimmed, " "); idx > 0 {
					if stamp, err := strconv.ParseInt(trimmed[idx+1:], 10, 64); err == nil {
						date = iso(stamp)
						ident = trimmed[:idx]
					}
				}
			}
			name, email := ident, ""
			if lt := strings.LastIndex(ident, "<"); lt > 0 {
				name = strings.TrimSpace(ident[:lt])
				email = strings.TrimSuffix(ident[lt+1:], ">")
			}
			message := view.Message
			if strings.HasSuffix(message, "\n") {
				message = message[:len(message)-1]
			}
			parents := make([]map[string]any, 0, len(view.Parents))
			for _, parent := range view.Parents {
				parents = append(parents, map[string]any{"sha": parent})
			}
			out = append(out, map[string]any{
				"sha":     oid,
				"node_id": b64("Commit:" + oid),
				"commit": map[string]any{
					"author":        map[string]any{"name": name, "email": email, "date": date},
					"committer":     map[string]any{"name": name, "email": email, "date": date},
					"message":       message,
					"tree":          map[string]any{"sha": view.Tree},
					"url":           "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/commits/" + oid,
					"comment_count": 0,
					"verification": map[string]any{
						"verified": false, "reason": "unsigned", "signature": nil, "payload": nil,
					},
				},
				"url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + oid,
				"html_url": "https://github.com/" + repo.Owner + "/" + repo.Name + "/commit/" + oid,
				"parents":  parents,
			})
			queue = append(queue, view.Parents...)
		}
	}
	w.writeObj(req, 200, out, nil)
}

func createGitRef(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	ref := bodyStr(body, "ref", "")
	sha := bodyStr(body, "sha", "")
	if !strings.HasPrefix(ref, "refs/") || !isOID(sha) {
		w.writeErr(req, 422, "Validation Failed")
		return
	}
	s.mu.RLock()
	_, exists := repo.Refs[ref]
	s.mu.RUnlock()
	if exists {
		w.writeErr(req, 422, "Reference already exists")
		return
	}
	object, ok := s.getObject(sha)
	if !ok {
		w.writeErr(req, 422, "Object does not exist")
		return
	}
	s.setRef(repo, ref, sha)
	w.writeObj(req, 201, map[string]any{
		"ref":     ref,
		"node_id": b64("Ref:" + ref),
		"url":     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/" + strings.TrimPrefix(ref, "refs/"),
		"object": map[string]any{
			"sha":  sha,
			"type": string(object.Type),
			"url":  "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/commits/" + sha,
		},
	}, nil)
}

func getGitRef(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	branch := req.Segments[8]
	s.mu.RLock()
	sha, ok := repo.Refs["refs/heads/"+branch]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	w.writeObj(req, 200, map[string]any{
		"ref":     "refs/heads/" + branch,
		"node_id": b64("Ref:refs/heads/" + branch),
		"url":     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/heads/" + branch,
		"object": map[string]any{
			"sha":  sha,
			"type": "commit",
			"url":  "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/commits/" + sha,
		},
	}, nil)
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

func contentsPath(req *Request) string {
	if len(req.Segments) <= 6 {
		return ""
	}
	return strings.Join(req.Segments[6:], "/")
}

// resolveRefCommit resolves the commit a contents read works against:
// a branch tip or an oid.
func resolveRefCommit(s *Store, repo *Repo, req *Request) (string, bool) {
	ref := req.Query.Get("ref")
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ref == "" {
		tip, ok := repo.Refs[s.headTarget(repo)]
		return tip, ok
	}
	if tip, ok := repo.Refs["refs/heads/"+ref]; ok {
		return tip, true
	}
	if tip, ok := repo.Refs[ref]; ok {
		return tip, true
	}
	if isOID(ref) {
		if object, ok := s.objects[ref]; ok && object.Type == objCommit {
			return ref, true
		}
	}
	return "", false
}

func dirEntryJSON(s *Store, repo *Repo, dir string, entry TreeEntry) map[string]any {
	path := entry.Name
	if dir != "" {
		path = dir + "/" + entry.Name
	}
	isDir := entry.Mode == "40000"
	var size int
	if !isDir {
		if blob, ok := s.getObject(entry.OID); ok {
			size = len(blob.Data)
		}
	}
	kind := "file"
	gitKind := "blobs"
	if isDir {
		kind = "dir"
		gitKind = "trees"
	}
	var download any
	if !isDir {
		download = "https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/" + repo.DefaultBranch + "/" + path
	}
	return map[string]any{
		"type":         kind,
		"size":         size,
		"name":         entry.Name,
		"path":         path,
		"sha":          entry.OID,
		"url":          "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
		"git_url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/" + gitKind + "/" + entry.OID,
		"html_url":     "https://github.com/" + repo.Owner + "/" + repo.Name + "/" + kind + "/" + repo.DefaultBranch + "/" + path,
		"download_url": download,
	}
}

func getContents(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	path := contentsPath(req)
	tip, ok := resolveRefCommit(s, repo, req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	commit, ok := s.getObject(tip)
	if !ok || commit.Type != objCommit {
		w.writeErr(req, 404, "Not Found")
		return
	}
	treeOID := parseCommit(commit.Data).Tree
	if path == "" {
		s.mu.RLock()
		entries := s.listDirLocked(treeOID, "")
		s.mu.RUnlock()
		if entries == nil {
			w.writeErr(req, 404, "Not Found")
			return
		}
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			out = append(out, dirEntryJSON(s, repo, "", entry))
		}
		w.writeObj(req, 200, out, nil)
		return
	}
	s.mu.RLock()
	found := s.readPathLocked(treeOID, path)
	var dirEntries []TreeEntry
	var blobData []byte
	var blobOID string
	if found != nil && found.Mode == "40000" {
		dirEntries = s.listDirLocked(treeOID, path)
	} else if found != nil {
		if blob, ok := s.objects[found.OID]; ok {
			blobData = blob.Data
			blobOID = found.OID
		}
	}
	s.mu.RUnlock()
	if found == nil {
		w.writeErr(req, 404, "Not Found")
		return
	}
	if found.Mode == "40000" {
		if dirEntries == nil {
			w.writeErr(req, 404, "Not Found")
			return
		}
		out := make([]map[string]any, 0, len(dirEntries))
		for _, entry := range dirEntries {
			out = append(out, dirEntryJSON(s, repo, path, entry))
		}
		w.writeObj(req, 200, out, nil)
		return
	}
	if blobData == nil {
		w.writeErr(req, 404, "Not Found")
		return
	}
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	w.writeObj(req, 200, map[string]any{
		"type":         "file",
		"encoding":     "base64",
		"size":         len(blobData),
		"name":         name,
		"path":         path,
		"content":      base64.StdEncoding.EncodeToString(blobData),
		"sha":          blobOID,
		"url":          "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
		"git_url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/blobs/" + blobOID,
		"html_url":     "https://github.com/" + repo.Owner + "/" + repo.Name + "/blob/" + repo.DefaultBranch + "/" + path,
		"download_url": "https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/" + repo.DefaultBranch + "/" + path,
	}, nil)
}

func putContents(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	path := contentsPath(req)
	message := bodyStr(body, "message", "Update file")
	rawContent, present := body["content"]
	if !present {
		w.writeErr(req, 422, "Validation Failed: content is required")
		return
	}
	contentB64, isStr := rawContent.(string)
	if !isStr {
		w.writeErr(req, 422, "Validation Failed: content is required")
		return
	}
	data, _ := base64.StdEncoding.DecodeString(contentB64)
	branch := bodyStr(body, "branch", "")
	if branch == "" {
		branch = req.Query.Get("branch")
	}
	if branch == "" {
		branch = repo.DefaultBranch
	}
	ref := "refs/heads/" + branch
	s.mu.RLock()
	tip, hasTip := repo.Refs[ref]
	s.mu.RUnlock()
	var currentSha string
	hasCurrent := false
	if hasTip {
		if commit, ok := s.getObject(tip); ok && commit.Type == objCommit {
			s.mu.RLock()
			found := s.readPathLocked(parseCommit(commit.Data).Tree, path)
			s.mu.RUnlock()
			if found != nil && found.Mode != "40000" {
				currentSha = found.OID
				hasCurrent = true
			}
		}
	}
	givenSha := bodyStr(body, "sha", "")
	if hasCurrent {
		if givenSha == "" || !strings.EqualFold(givenSha, currentSha) {
			w.writeErr(req, 409, "Conflict: sha does not match the current blob")
			return
		}
	}
	s.mu.Lock()
	blobOID := s.putObjectLocked(objBlob, data)
	commitOID := s.commitFilesLocked(repo, branch, []fileChange{{path: path, data: data}}, message)
	s.mu.Unlock()
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	status := 201
	if hasCurrent {
		status = 200
	}
	w.writeObj(req, status, map[string]any{
		"content": map[string]any{
			"name":         name,
			"path":         path,
			"sha":          blobOID,
			"size":         len(data),
			"url":          "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + path,
			"git_url":      "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/blobs/" + blobOID,
			"html_url":     "https://github.com/" + repo.Owner + "/" + repo.Name + "/blob/" + branch + "/" + path,
			"download_url": "https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/" + branch + "/" + path,
			"type":         "file",
			"encoding":     "base64",
		},
		"commit": map[string]any{
			"sha":       commitOID,
			"node_id":   b64("Commit:" + commitOID),
			"message":   message,
			"url":       "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + commitOID,
			"author":    map[string]any{"name": s.viewer.Name, "email": s.viewer.Email, "date": repo.CreatedAt},
			"committer": map[string]any{"name": s.viewer.Name, "email": s.viewer.Email, "date": repo.CreatedAt},
			"tree":      map[string]any{"sha": ""},
			"parents":   []any{},
		},
	}, nil)
}

func deleteContents(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	path := contentsPath(req)
	message := bodyStr(body, "message", "Delete file")
	branch := bodyStr(body, "branch", "")
	if branch == "" {
		branch = req.Query.Get("branch")
	}
	if branch == "" {
		branch = repo.DefaultBranch
	}
	s.mu.RLock()
	tip, hasTip := repo.Refs["refs/heads/"+branch]
	s.mu.RUnlock()
	if !hasTip {
		w.writeErr(req, 404, "Not Found")
		return
	}
	commit, ok := s.getObject(tip)
	if !ok || commit.Type != objCommit {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	found := s.readPathLocked(parseCommit(commit.Data).Tree, path)
	s.mu.RUnlock()
	if found == nil || found.Mode == "40000" {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.Lock()
	commitOID := s.commitFilesLocked(repo, branch, []fileChange{{path: path, data: nil}}, message)
	s.mu.Unlock()
	w.writeObj(req, 200, map[string]any{
		"commit": map[string]any{
			"sha":     commitOID,
			"node_id": b64("Commit:" + commitOID),
			"message": message,
			"url":     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + commitOID,
		},
	}, nil)
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func createLabel(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	name := bodyStr(body, "name", "")
	if name == "" {
		w.writeErr(req, 422, "Validation Failed: name is required")
		return
	}
	s.mu.Lock()
	existing, ok := repo.Labels[name]
	if ok {
		s.mu.Unlock()
		w.writeJSON(req, 201, labelJSONBytes(existing), nil)
		return
	}
	label := &Label{Name: name, Color: bodyStr(body, "color", "ededed")}
	repo.Labels[name] = label
	s.mu.Unlock()
	w.writeJSON(req, 201, labelJSONBytes(label), nil)
}

func listLabels(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	s.mu.RLock()
	names := make([]string, 0, len(repo.Labels))
	for name := range repo.Labels {
		names = append(names, name)
	}
	s.mu.RUnlock()
	sort.Strings(names)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, name := range names {
		if i > 0 {
			buf.WriteByte(',')
		}
		s.mu.RLock()
		label := repo.Labels[name]
		s.mu.RUnlock()
		buf.WriteString(labelJSONBytes(label))
	}
	buf.WriteByte(']')
	w.writeJSON(req, 200, buf.String(), nil)
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

func createIssue(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	title := bodyStr(body, "title", "")
	if title == "" {
		w.writeErr(req, 422, "Validation Failed: title is required")
		return
	}
	var wantedLabels []string
	if raw, ok := body["labels"].([]any); ok {
		for _, entry := range raw {
			switch v := entry.(type) {
			case string:
				wantedLabels = append(wantedLabels, v)
			case map[string]any:
				wantedLabels = append(wantedLabels, bodyStr(v, "name", ""))
			}
		}
	}
	s.mu.RLock()
	for _, name := range wantedLabels {
		if _, ok := repo.Labels[name]; !ok {
			s.mu.RUnlock()
			w.writeErr(req, 422, `Validation Failed: label "`+name+`" does not exist`)
			return
		}
	}
	s.mu.RUnlock()
	issue := s.createIssue(repo, title, bodyStr(body, "body", ""), wantedLabels)
	// The cached JSON string goes out as-is: no parse + re-stringify.
	w.writeJSON(req, 201, issueJSON(s, repo, issue), nil)
}

func listIssues(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	state := req.Query.Get("state")
	if state == "" {
		state = "open"
	}
	sortMode := req.Query.Get("sort")
	if sortMode == "" {
		sortMode = "created"
	}
	direction := req.Query.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage, page := pageOf(req)
	s.mu.RLock()
	items := make([]*Issue, 0, len(repo.Issues))
	for _, issue := range repo.Issues {
		if state != "all" && issue.State != state {
			continue
		}
		items = append(items, issue)
	}
	s.mu.RUnlock()
	if sortMode == "created" {
		sort.Slice(items, func(i, j int) bool {
			if direction == "asc" {
				return items[i].CreatedSeq < items[j].CreatedSeq
			}
			return items[i].CreatedSeq > items[j].CreatedSeq
		})
	}
	start, end := paginateLen(len(items), perPage, page)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			buf.WriteByte(',')
		}
		buf.WriteString(issueJSON(s, repo, items[i]))
	}
	buf.WriteByte(']')
	w.writeJSON(req, 200, buf.String(), nil)
}

func getIssue(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, err := strconv.Atoi(req.Segments[6])
	if err != nil {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	issue, ok := repo.Issues[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	// The hot read path: the cached JSON string goes out as-is.
	w.writeJSON(req, 200, issueJSON(s, repo, issue), nil)
}

func patchIssue(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, err := strconv.Atoi(req.Segments[6])
	if err != nil {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	issue, ok := repo.Issues[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	s.mu.Lock()
	if title, ok := body["title"].(string); ok {
		issue.Title = title
	}
	if b, ok := body["body"].(string); ok {
		issue.Body = b
	}
	if state, ok := body["state"].(string); ok && (state == "open" || state == "closed") {
		issue.State = state
		if state == "closed" {
			issue.ClosedAt = iso(epochBase + s.clock.next())
		} else {
			issue.ClosedAt = ""
		}
	}
	issue.JSONCache = ""
	repo.JSONCache = ""
	s.mu.Unlock()
	w.writeJSON(req, 200, issueJSON(s, repo, issue), nil)
}

func issueNumber(req *Request) (int, bool) {
	number, err := strconv.Atoi(req.Segments[6])
	if err != nil {
		return 0, false
	}
	return number, true
}

func createIssueComment(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, ok := issueNumber(req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	issue, ok := repo.Issues[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	comment := s.addComment(repo, issue, bodyStr(body, "body", ""))
	w.writeObj(req, 201, commentJSON(s, repo, issue, comment), nil)
}

func listIssueComments(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, ok := issueNumber(req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	issue, ok := repo.Issues[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	comments := issue.Comments
	s.mu.RUnlock()
	out := make([]map[string]any, 0, len(comments))
	for _, comment := range comments {
		out = append(out, commentJSON(s, repo, issue, comment))
	}
	w.writeObj(req, 200, out, nil)
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

func createPull(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	body := bodyObj(req)
	if body == nil {
		w.writeErr(req, 400, "Invalid JSON")
		return
	}
	head := bodyStr(body, "head", "")
	base := bodyStr(body, "base", repo.DefaultBranch)
	title := bodyStr(body, "title", "")
	if head == "" || title == "" {
		w.writeErr(req, 422, "Validation Failed: head and title are required")
		return
	}
	s.mu.RLock()
	_, hasHead := repo.Refs["refs/heads/"+head]
	_, hasBase := repo.Refs["refs/heads/"+base]
	s.mu.RUnlock()
	if !hasHead || !hasBase {
		w.writeErr(req, 422, "Validation Failed: head or base branch does not exist")
		return
	}
	s.mu.RLock()
	for _, existing := range repo.Pulls {
		if existing.HeadRef == head && existing.State == "open" {
			s.mu.RUnlock()
			w.writeErr(req, 422, "Validation Failed: a pull request already exists for this head branch")
			return
		}
	}
	s.mu.RUnlock()
	pull := s.createPull(repo, title, bodyStr(body, "body", ""), head, base)
	if pull == nil {
		w.writeErr(req, 422, "Validation Failed: head or base branch does not exist")
		return
	}
	w.writeJSON(req, 201, pullJSON(s, repo, pull), nil)
}

func listPulls(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	state := req.Query.Get("state")
	if state == "" {
		state = "open"
	}
	perPage, page := pageOf(req)
	s.mu.RLock()
	items := make([]*Pull, 0, len(repo.Pulls))
	for _, pull := range repo.Pulls {
		if state != "all" && pull.State != state {
			continue
		}
		items = append(items, pull)
	}
	s.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedSeq > items[j].CreatedSeq })
	start, end := paginateLen(len(items), perPage, page)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := start; i < end; i++ {
		if i > start {
			buf.WriteByte(',')
		}
		buf.WriteString(pullJSON(s, repo, items[i]))
	}
	buf.WriteByte(']')
	w.writeJSON(req, 200, buf.String(), nil)
}

func getPull(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, ok := issueNumber(req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	pull, ok := repo.Pulls[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	w.writeJSON(req, 200, pullJSON(s, repo, pull), nil)
}

// diffPullFiles returns the merge-base tree against the head tree, as a
// changed-file list.
func diffPullFiles(s *Store, pull *Pull) []ChangedFile {
	s.mu.RLock()
	mb := s.mergeBaseLocked(pull.HeadSha, pull.BaseSha)
	baseTree := emptyTreeOID
	if mb != "" {
		if mbObject, ok := s.objects[mb]; ok && mbObject.Type == objCommit {
			baseTree = parseCommit(mbObject.Data).Tree
		}
	}
	headObject, ok := s.objects[pull.HeadSha]
	if !ok || headObject.Type != objCommit {
		s.mu.RUnlock()
		return nil
	}
	headTree := parseCommit(headObject.Data).Tree
	files := s.diffTreesLocked(baseTree, headTree)
	s.mu.RUnlock()
	return files
}

func listPullFiles(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, ok := issueNumber(req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	pull, ok := repo.Pulls[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	files := diffPullFiles(s, pull)
	out := make([]map[string]any, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]any{
			"sha":          "",
			"filename":     file.Filename,
			"status":       file.Status,
			"additions":    file.Additions,
			"deletions":    file.Deletions,
			"changes":      file.Changes,
			"blob_url":     "",
			"raw_url":      "",
			"contents_url": "",
			"patch":        "",
		})
	}
	w.writeObj(req, 200, out, nil)
}

func mergePull(w *Responder, req *Request, s *Store) {
	if !requireAuth(w, req, s) {
		return
	}
	if !jsonOk(w, req) {
		return
	}
	repo := lookupRepo(w, req, s)
	if repo == nil {
		return
	}
	number, ok := issueNumber(req)
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	pull, ok := repo.Pulls[number]
	s.mu.RUnlock()
	if !ok {
		w.writeErr(req, 404, "Not Found")
		return
	}
	s.mu.RLock()
	closed := pull.State == "closed"
	s.mu.RUnlock()
	if closed {
		w.writeErr(req, 405, "Pull request has already been merged or closed")
		return
	}
	s.mu.Lock()
	mergeSha := s.squashMergeLocked(repo, pull)
	if mergeSha == "" {
		s.mu.Unlock()
		w.writeErr(req, 405, "Merge conflict: the pull request cannot be merged")
		return
	}
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = iso(epochBase + s.clock.next())
	pull.MergeCommit = mergeSha
	s.mu.Unlock()
	w.writeObj(req, 200, map[string]any{
		"sha":     mergeSha,
		"merged":  true,
		"message": "Pull request successfully merged",
	}, nil)
}

// ---------------------------------------------------------------------------
// dispatch
// ---------------------------------------------------------------------------

// dispatch routes one request. The segment table mirrors the reference
// forge's router: REST under /api/v3, git smart HTTP at /:owner/:repo,
// GraphQL at /api/graphql, MCP at /mcp, and a JSON 404 for the rest.
// A path starting with /api never falls through to a git route.
func dispatch(w *Responder, req *Request, s *Store) {
	segs := req.Segments
	method := req.Method

	if len(segs) == 1 && segs[0] == "healthz" && method == "GET" {
		w.writeObj(req, 200, map[string]any{"ok": true}, nil)
		return
	}
	if len(segs) >= 1 && segs[0] == "api" {
		dispatchAPI(w, req, s, segs, method)
		return
	}
	if len(segs) == 1 && segs[0] == "mcp" && method == "POST" {
		mcpHandler(w, req, s)
		return
	}
	if len(segs) == 4 && segs[2] == "info" && segs[3] == "refs" && method == "GET" {
		infoRefs(w, req, s)
		return
	}
	if len(segs) == 3 && segs[2] == "git-upload-pack" && method == "POST" {
		uploadPack(w, req, s)
		return
	}
	if len(segs) == 3 && segs[2] == "git-receive-pack" && method == "POST" {
		receivePack(w, req, s)
		return
	}
	w.writeErr(req, 404, "Not Found")
}

func dispatchAPI(w *Responder, req *Request, s *Store, segs []string, method string) {
	// /api/v3, /api/v3/meta, /api/v3/user, /api/graphql
	if len(segs) == 2 && segs[1] == "v3" && method == "GET" {
		apiRoot(w, req, s)
		return
	}
	if len(segs) == 2 && segs[1] == "graphql" && method == "POST" {
		graphqlHandler(w, req, s)
		return
	}
	if len(segs) == 3 && segs[1] == "v3" && segs[2] == "meta" && method == "GET" {
		apiMeta(w, req, s)
		return
	}
	if len(segs) == 3 && segs[1] == "v3" && segs[2] == "user" && method == "GET" {
		getUser(w, req, s)
		return
	}
	if len(segs) == 4 && segs[1] == "v3" && segs[2] == "user" && segs[3] == "repos" {
		if method == "POST" {
			createRepo(w, req, s)
			return
		}
		if method == "GET" {
			listUserRepos(w, req, s)
			return
		}
	}
	if len(segs) >= 5 && segs[1] == "v3" && segs[2] == "repos" {
		dispatchRepo(w, req, s, segs, method)
		return
	}
	w.writeErr(req, 404, "Not Found")
}

func dispatchRepo(w *Responder, req *Request, s *Store, segs []string, method string) {
	// /api/v3/repos/:owner/:repo[/...]
	if len(segs) == 5 {
		switch method {
		case "GET":
			getRepo(w, req, s)
		case "PATCH":
			patchRepo(w, req, s)
		case "DELETE":
			deleteRepo(w, req, s)
		default:
			w.writeErr(req, 404, "Not Found")
		}
		return
	}
	rest := segs[5:]
	switch rest[0] {
	case "branches":
		if len(rest) == 1 && method == "GET" {
			listBranches(w, req, s)
			return
		}
		if len(rest) == 2 && method == "GET" {
			getBranch(w, req, s)
			return
		}
	case "commits":
		if len(rest) == 1 && method == "GET" {
			listCommits(w, req, s)
			return
		}
	case "git":
		if len(rest) == 2 && rest[1] == "refs" && method == "POST" {
			createGitRef(w, req, s)
			return
		}
		if len(rest) == 4 && rest[1] == "refs" && rest[2] == "heads" && method == "GET" {
			getGitRef(w, req, s)
			return
		}
	case "contents":
		if method == "GET" {
			getContents(w, req, s)
			return
		}
		if method == "PUT" {
			putContents(w, req, s)
			return
		}
		if method == "DELETE" {
			deleteContents(w, req, s)
			return
		}
	case "labels":
		if len(rest) == 1 && method == "POST" {
			createLabel(w, req, s)
			return
		}
		if len(rest) == 1 && method == "GET" {
			listLabels(w, req, s)
			return
		}
	case "issues":
		if len(rest) == 1 && method == "POST" {
			createIssue(w, req, s)
			return
		}
		if len(rest) == 1 && method == "GET" {
			listIssues(w, req, s)
			return
		}
		if len(rest) == 2 {
			if method == "GET" {
				getIssue(w, req, s)
				return
			}
			if method == "PATCH" {
				patchIssue(w, req, s)
				return
			}
		}
		if len(rest) == 3 && rest[2] == "comments" {
			if method == "POST" {
				createIssueComment(w, req, s)
				return
			}
			if method == "GET" {
				listIssueComments(w, req, s)
				return
			}
		}
	case "pulls":
		if len(rest) == 1 && method == "POST" {
			createPull(w, req, s)
			return
		}
		if len(rest) == 1 && method == "GET" {
			listPulls(w, req, s)
			return
		}
		if len(rest) == 2 && method == "GET" {
			getPull(w, req, s)
			return
		}
		if len(rest) == 3 && rest[2] == "files" && method == "GET" {
			listPullFiles(w, req, s)
			return
		}
		if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
			mergePull(w, req, s)
			return
		}
	}
	w.writeErr(req, 404, "Not Found")
}
