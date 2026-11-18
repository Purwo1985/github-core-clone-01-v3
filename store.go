// The forge's store: one in-memory arena of git objects plus the REST
// resources (repositories, issues, pull requests, labels) that live on top
// of it. Objects are immutable once stored, so packs and advertisements
// built from them stay valid until a ref moves; a per-repository version
// counter invalidates exactly the caches a ref change affects.
//
// Timestamps never read the wall clock: every timestamp the forge invents
// comes from MAKEFUN_TRIAL_NOW plus a monotonic counter, so two runs on
// the same trial answer identically.

package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// epochBase is the fixed epoch the suite's issue/pull timestamps derive
// from (the same constant the reference implementation uses).
const epochBase int64 = 1735689600

// iso renders whole seconds as an ISO-8601 UTC timestamp with millis,
// the shape JavaScript's Date.toISOString produces.
func iso(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// ForgeClock is a deterministic clock: trial time plus a counter.
type ForgeClock struct {
	baseSec int64
	seq     int64
}

func newForgeClock(trialNowSec int64) *ForgeClock {
	return &ForgeClock{baseSec: trialNowSec}
}

func (c *ForgeClock) next() int64 {
	c.seq++
	return c.baseSec + c.seq
}

func (c *ForgeClock) iso() string {
	return iso(c.next())
}

// Comment is one issue/pull-request comment.
type Comment struct {
	ID   int
	Body string
}

// Issue is one repository issue.
type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	Comments   []Comment
	CreatedSeq int64
	ClosedAt   string // "" means null
	JSONCache  string // rendered issue JSON, "" when stale
}

// Pull is one pull request.
type Pull struct {
	ID          int
	Number      int
	Title       string
	Body        string
	State       string // "open" | "closed"
	Merged      bool
	MergedAt    string // "" means null
	MergeCommit string // "" means null
	HeadRef     string
	BaseRef     string
	HeadSha     string
	BaseSha     string
	CreatedSeq  int64
	Comments    []Comment
}

// Label is one repository label.
type Label struct {
	Name  string
	Color string
}

// LsRefRow is one row of an ls-refs / advertisement listing.
type LsRefRow struct {
	OID          string
	Name         string
	Peeled       string
	SymrefTarget string
	Unborn       bool
}

// Repo is one repository.
type Repo struct {
	ID            int
	Name          string
	Owner         string
	IsPrivate     bool
	Description   string
	DefaultBranch string
	Refs          map[string]string
	RefVersion    int
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	Labels        map[string]*Label
	NextNumber    int
	CreatedAt     string
	JSONCache     string
	LsRefsCache   []LsRefRow
}

// ChangedFile is one file of a tree diff.
type ChangedFile struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
}

// Store is the whole forge state. One RWMutex guards it: the hot paths
// are reads (clone packs are built under a read lock; objects never
// change), mutations take the write lock.
type Store struct {
	mu        sync.RWMutex
	packMu    sync.Mutex // serialises pack builds: one build per cache key
	clock     *ForgeClock
	viewer    Identity
	token     string
	objects   map[string]GitObject
	repos     map[string]*Repo
	repoID    int
	itemID    int
	commentID int
	packCache map[string][]byte
}

func newStore(viewer Identity, token string, trialNowSec int64) *Store {
	return &Store{
		clock:     newForgeClock(trialNowSec),
		viewer:    viewer,
		token:     token,
		objects:   map[string]GitObject{},
		repos:     map[string]*Repo{},
		packCache: map[string][]byte{},
	}
}

func (s *Store) nextRepoID() int {
	s.repoID++
	return s.repoID
}

func (s *Store) nextItemID() int {
	s.itemID++
	return s.itemID
}

func (s *Store) nextCommentID() int {
	s.commentID++
	return s.commentID
}

// ------------------------------------------------------------------ objects

func (s *Store) putObjectLocked(typ ObjType, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		s.objects[oid] = GitObject{Type: typ, Data: cp}
	}
	return oid
}

// putObject stores an object (or finds the identical one) and returns its id.
func (s *Store) putObject(typ ObjType, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putObjectLocked(typ, data)
}

func (s *Store) getObject(oid string) (GitObject, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[oid]
	return obj, ok
}

// getObjectLocked reads without locking (caller holds at least a read lock).
func (s *Store) getObjectLocked(oid string) (GitObject, bool) {
	obj, ok := s.objects[oid]
	return obj, ok
}

// ------------------------------------------------------------------ repos

func repoKey(owner, name string) string {
	return owner + "/" + name
}

func (s *Store) createRepo(owner, name string, isPrivate bool, description string) *Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo := &Repo{
		ID:            s.nextRepoID(),
		Name:          name,
		Owner:         owner,
		IsPrivate:     isPrivate,
		Description:   description,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		Labels:        map[string]*Label{},
		NextNumber:    1,
		CreatedAt:     s.clock.iso(),
	}
	s.repos[repoKey(owner, name)] = repo
	return repo
}

// getRepo returns a repository only when it belongs to the token's login;
// anything else is a 404.
func (s *Store) getRepo(owner, name string) *Repo {
	if owner != s.viewer.Login {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[repoKey(owner, name)]
}

func (s *Store) deleteRepo(repo *Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos, repoKey(repo.Owner, repo.Name))
	prefix := itoa(repo.ID) + "|"
	for key := range s.packCache {
		if strings.HasPrefix(key, prefix) {
			delete(s.packCache, key)
		}
	}
}

// ------------------------------------------------------------ issues / pulls

// createIssue creates an issue; issues and pull requests share one
// number counter.
func (s *Store) createIssue(repo *Repo, title, body string, labels []string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	issue := &Issue{
		ID:         s.nextItemID(),
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     labels,
		CreatedSeq: s.clock.next(),
	}
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.JSONCache = ""
	return issue
}

func (s *Store) closeIssue(issue *Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue.State == "closed" {
		return
	}
	issue.State = "closed"
	issue.ClosedAt = s.clock.iso()
	issue.JSONCache = ""
}

func (s *Store) addComment(repo *Repo, issue *Issue, body string) Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	comment := Comment{ID: s.nextCommentID(), Body: body}
	issue.Comments = append(issue.Comments, comment)
	issue.JSONCache = ""
	repo.JSONCache = ""
	return comment
}

// addPullComment appends a review comment to a pull request (same id
// sequence as issue comments, as in the reference forge).
func (s *Store) addPullComment(pull *Pull, body string) Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	comment := Comment{ID: s.nextCommentID(), Body: body}
	pull.Comments = append(pull.Comments, comment)
	return comment
}

// createPull creates a pull request, or nil when head/base is missing or
// a live PR already covers the head.
func (s *Store) createPull(repo *Repo, title, body, head, base string) *Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	headSha, ok1 := repo.Refs["refs/heads/"+head]
	baseSha, ok2 := repo.Refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return nil
	}
	for _, existing := range repo.Pulls {
		if existing.HeadRef == head && existing.State == "open" {
			return nil
		}
	}
	pull := &Pull{
		ID:         s.nextItemID(),
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    head,
		BaseRef:    base,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		CreatedSeq: s.clock.next(),
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	return pull
}

// mergePull squash-merges an open pull request; returns the merge sha,
// or "" when closed or conflicting.
func (s *Store) mergePull(repo *Repo, pull *Pull) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pull.State == "closed" {
		return ""
	}
	mergeSha := s.squashMergeLocked(repo, pull)
	if mergeSha == "" {
		return ""
	}
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = iso(epochBase + s.clock.next())
	pull.MergeCommit = mergeSha
	return mergeSha
}

// getRef reads one ref (caller need not hold a lock).
func (s *Store) getRef(repo *Repo, ref string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := repo.Refs[ref]
	return oid, ok
}

// storeObjects inserts unpacked objects into the arena.
func (s *Store) storeObjects(objects map[string]GitObject) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for oid, object := range objects {
		if _, ok := s.objects[oid]; !ok {
			s.objects[oid] = object
		}
	}
}

// isAncestor is the locking wrapper for isAncestorLocked.
func (s *Store) isAncestor(ancestor, descendant string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isAncestorLocked(ancestor, descendant)
}

// ------------------------------------------------------------------ refs

// setRefLocked moves a ref and bumps the version that invalidates the
// ref caches (caller holds the write lock).
func (s *Store) setRefLocked(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
	repo.RefVersion++
	repo.LsRefsCache = nil
}

func (s *Store) setRef(repo *Repo, ref, oid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRefLocked(repo, ref, oid)
}

func (s *Store) deleteRef(repo *Repo, ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(repo.Refs, ref)
	repo.RefVersion++
	repo.LsRefsCache = nil
}

func (s *Store) headTarget(repo *Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

func (s *Store) headOid(repo *Repo) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := repo.Refs[s.headTarget(repo)]
	return oid, ok
}

// ------------------------------------------------------------------ reachability

type reachOpts struct {
	depth    int // -1 means no limit
	hasDepth bool
	exclude  map[string]bool
	stopAt   map[string]bool
}

// reachable returns every object reachable from roots. With a depth
// limit, commits at distance depth-1 are the shallow boundary: their
// trees and blobs ship, their parents do not. exclude removes a whole
// closure up front (the client's haves); stopAt cuts the exclude walk
// at shallow boundaries the client holds but whose parents it does not.
func (s *Store) reachableLocked(roots []string, opts reachOpts) map[string]bool {
	seen := map[string]bool{}
	visited := map[string]bool{}
	commitDepth := map[string]int{}
	queue := make([]string, 0, len(roots))
	for _, root := range roots {
		if !isOID(root) || root == zeroOID {
			continue
		}
		queue = append(queue, root)
		commitDepth[root] = 0
	}
	head := 0
	for head < len(queue) {
		oid := queue[head]
		head++
		if visited[oid] {
			continue
		}
		visited[oid] = true
		object, ok := s.objects[oid]
		if !ok {
			continue
		}
		if !opts.exclude[oid] {
			seen[oid] = true
		}
		switch object.Type {
		case objCommit:
			view := parseCommit(object.Data)
			d := commitDepth[oid]
			queue = append(queue, view.Tree)
			cut := opts.hasDepth && d >= opts.depth-1
			stopped := opts.stopAt[oid]
			if !cut && !stopped {
				for _, parent := range view.Parents {
					if _, ok := commitDepth[parent]; !ok {
						commitDepth[parent] = d + 1
					}
					queue = append(queue, parent)
				}
			}
		case objTag:
			queue = append(queue, parseTag(object.Data).Object)
		case objTree:
			for _, entry := range parseTree(object.Data) {
				if entry.Mode == "160000" {
					continue
				}
				queue = append(queue, entry.OID)
			}
		}
	}
	return seen
}

// shallowBoundary returns the shallow-boundary commits of a
// depth-limited walk from roots (distance depth-1).
func (s *Store) shallowBoundaryLocked(roots []string, depth int) []string {
	var boundary []string
	seen := map[string]bool{}
	type item struct {
		oid string
		d   int
	}
	queue := make([]item, 0, len(roots))
	for _, oid := range roots {
		if isOID(oid) && oid != zeroOID {
			queue = append(queue, item{oid, 0})
		}
	}
	head := 0
	for head < len(queue) {
		cur := queue[head]
		head++
		if seen[cur.oid] {
			continue
		}
		seen[cur.oid] = true
		object, ok := s.objects[cur.oid]
		if !ok || object.Type != objCommit {
			continue
		}
		if cur.d == depth-1 {
			boundary = append(boundary, cur.oid)
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queue = append(queue, item{parent, cur.d + 1})
		}
	}
	return boundary
}

// isAncestorLocked reports whether ancestor is reachable from descendant
// through parents (caller holds at least a read lock).
func (s *Store) isAncestorLocked(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := s.objects[oid]
		if !ok || object.Type != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// mergeBaseLocked returns the nearest common ancestor of two commits,
// or "" (caller holds at least a read lock).
func (s *Store) mergeBaseLocked(a, b string) string {
	fromA := map[string]bool{}
	queueA := []string{a}
	for len(queueA) > 0 {
		oid := queueA[len(queueA)-1]
		queueA = queueA[:len(queueA)-1]
		if fromA[oid] {
			continue
		}
		fromA[oid] = true
		object, ok := s.objects[oid]
		if !ok || object.Type != objCommit {
			continue
		}
		queueA = append(queueA, parseCommit(object.Data).Parents...)
	}
	queueB := []string{b}
	seenB := map[string]bool{}
	for len(queueB) > 0 {
		oid := queueB[len(queueB)-1]
		queueB = queueB[:len(queueB)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if fromA[oid] {
			return oid
		}
		object, ok := s.objects[oid]
		if !ok || object.Type != objCommit {
			continue
		}
		queueB = append(queueB, parseCommit(object.Data).Parents...)
	}
	return ""
}

// mergeBase is the locking wrapper.
func (s *Store) mergeBase(a, b string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mergeBaseLocked(a, b)
}

// ------------------------------------------------------------------ trees

// flattenTreeLocked lists a tree flat: path -> entry (caller holds a lock).
func (s *Store) flattenTreeLocked(treeOID string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		object, ok := s.objects[oid]
		if !ok || object.Type != objTree {
			return
		}
		for _, entry := range parseTree(object.Data) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else if entry.Mode != "160000" {
				out[path] = entry
			}
		}
	}
	walk(treeOID, "")
	return out
}

// readPathLocked returns the entry at path under rootOID, or nil.
func (s *Store) readPathLocked(rootOID, path string) *TreeEntry {
	segments := splitNonEmpty(path, '/')
	current := rootOID
	for i, segment := range segments {
		object, ok := s.objects[current]
		if !ok || object.Type != objTree {
			return nil
		}
		var found *TreeEntry
		for _, entry := range parseTree(object.Data) {
			if entry.Name == segment {
				e := entry
				found = &e
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(segments)-1 {
			return found
		}
		if found.Mode != "40000" {
			return nil
		}
		current = found.OID
	}
	return &TreeEntry{OID: current, Mode: "40000"}
}

// listDirLocked lists a directory's entries ("" = the root tree).
func (s *Store) listDirLocked(rootOID, path string) []TreeEntry {
	if path == "" {
		object, ok := s.objects[rootOID]
		if !ok || object.Type != objTree {
			return nil
		}
		return parseTree(object.Data)
	}
	found := s.readPathLocked(rootOID, path)
	if found == nil || found.Mode != "40000" {
		return nil
	}
	object, ok := s.objects[found.OID]
	if !ok || object.Type != objTree {
		return nil
	}
	return parseTree(object.Data)
}

// setPathLocked returns a new tree with path set to blobOID (or removed
// when ""), built bottom-up from rootOID (caller holds the write lock).
func (s *Store) setPathLocked(rootOID, path, blobOID string) string {
	segments := splitNonEmpty(path, '/')
	var build func(treeOID string, depth int) string
	build = func(treeOID string, depth int) string {
		var entries []TreeEntry
		if object, ok := s.objects[treeOID]; ok && object.Type == objTree {
			entries = parseTree(object.Data)
		}
		name := segments[depth]
		next := make([]TreeEntry, 0, len(entries)+1)
		for _, entry := range entries {
			if entry.Name != name {
				next = append(next, entry)
			}
		}
		if depth == len(segments)-1 {
			if blobOID != "" {
				next = append(next, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
			}
		} else {
			var childOID string
			for _, entry := range entries {
				if entry.Name == name && entry.Mode == "40000" {
					childOID = entry.OID
					break
				}
			}
			if childOID == "" {
				childOID = emptyTreeOID
			}
			rebuilt := build(childOID, depth+1)
			isEmpty := rebuilt == emptyTreeOID
			if isEmpty {
				if obj, ok := s.objects[rebuilt]; ok && len(obj.Data) == 0 {
					isEmpty = true
				} else {
					isEmpty = false
				}
			}
			if !isEmpty {
				next = append(next, TreeEntry{Mode: "40000", Name: name, OID: rebuilt})
			}
		}
		return s.putObjectLocked(objTree, buildTree(next))
	}
	return build(rootOID, 0)
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

// lineDiffCounts is a small line diff: LCS length over lines.
func lineDiffCounts(oldData, newData []byte) (additions, deletions int) {
	split := func(data []byte) []string {
		if len(data) == 0 {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	a := split(oldData)
	b := split(newData)
	m, n := len(a), len(b)
	if m == 0 {
		return n, 0
	}
	if n == 0 {
		return 0, m
	}
	prev := make([]uint32, n+1)
	cur := make([]uint32, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	common := int(prev[n])
	return n - common, m - common
}

// diffTreesLocked diffs two trees, with line counts.
func (s *Store) diffTreesLocked(baseOID, headOID string) []ChangedFile {
	base := s.flattenTreeLocked(baseOID)
	head := s.flattenTreeLocked(headOID)
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var files []ChangedFile
	for _, path := range sorted {
		b, hasB := base[path]
		h, hasH := head[path]
		switch {
		case hasB && !hasH:
			data := s.objects[b.OID].Data
			deletions := countLines(data)
			files = append(files, ChangedFile{Filename: path, Status: "removed", Deletions: deletions, Changes: deletions})
		case !hasB && hasH:
			data := s.objects[h.OID].Data
			additions := countLines(data)
			files = append(files, ChangedFile{Filename: path, Status: "added", Additions: additions, Changes: additions})
		case hasB && hasH && b.OID != h.OID:
			add, del := lineDiffCounts(s.objects[b.OID].Data, s.objects[h.OID].Data)
			files = append(files, ChangedFile{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	return files
}

// diffTrees is the locking wrapper.
func (s *Store) diffTrees(baseOID, headOID string) []ChangedFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diffTreesLocked(baseOID, headOID)
}

// mergeableLocked reports MERGEABLE or CONFLICTING for a head tip
// against a base tip (caller holds a lock).
func (s *Store) mergeableLocked(headTip, baseTip string) string {
	mb := s.mergeBaseLocked(headTip, baseTip)
	if mb == "" {
		return "CONFLICTING"
	}
	mbObject, ok1 := s.objects[mb]
	headObject, ok2 := s.objects[headTip]
	baseObject, ok3 := s.objects[baseTip]
	if !ok1 || !ok2 || !ok3 {
		return "CONFLICTING"
	}
	mbTree := parseCommit(mbObject.Data).Tree
	headTree := parseCommit(headObject.Data).Tree
	baseTree := parseCommit(baseObject.Data).Tree
	mbFiles := s.flattenTreeLocked(mbTree)
	headFiles := s.flattenTreeLocked(headTree)
	baseFiles := s.flattenTreeLocked(baseTree)
	paths := map[string]bool{}
	for p := range mbFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	for p := range baseFiles {
		paths[p] = true
	}
	for path := range paths {
		m := mbFiles[path].OID
		h := headFiles[path].OID
		b := baseFiles[path].OID
		if h != m && b != m && h != b {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

// mergeable is the locking wrapper.
func (s *Store) mergeable(headTip, baseTip string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mergeableLocked(headTip, baseTip)
}

// ------------------------------------------------------------------ commits

// ident is the identity string for commit author/committer lines.
func (s *Store) ident() string {
	return s.viewer.Name + " <" + s.viewer.Email + ">"
}

// commitFilesLocked commits files (path -> bytes, nil to delete) on
// branch, creating the branch from the default tip when it does not
// exist yet. Returns the new commit oid (caller holds the write lock).
func (s *Store) commitFilesLocked(repo *Repo, branch string, files []fileChange, message string) string {
	ref := "refs/heads/" + branch
	tip, ok := repo.Refs[ref]
	if !ok {
		tip, ok = repo.Refs[s.headTarget(repo)]
	}
	treeOID := emptyTreeOID
	var parents []string
	if ok {
		if commit, ok := s.objects[tip]; ok && commit.Type == objCommit {
			treeOID = parseCommit(commit.Data).Tree
			parents = append(parents, tip)
		}
	}
	for _, file := range files {
		var blobOID string
		if file.data != nil {
			blobOID = s.putObjectLocked(objBlob, file.data)
		}
		treeOID = s.setPathLocked(treeOID, file.path, blobOID)
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	commitData := buildCommit(treeOID, parents, message, s.ident(), s.ident(), s.clock.next())
	commitOID := s.putObjectLocked(objCommit, commitData)
	s.setRefLocked(repo, ref, commitOID)
	return commitOID
}

// fileChange is one path update for commitFiles.
type fileChange struct {
	path string
	data []byte // nil deletes the path
}

// squashMergeLocked squash-merges pull onto its base branch: one commit
// whose tree is the head tree, parented on the base tip. Returns the
// merge commit oid, or "" when the pull conflicts (caller holds the
// write lock).
func (s *Store) squashMergeLocked(repo *Repo, pull *Pull) string {
	headTip, ok1 := repo.Refs["refs/heads/"+pull.HeadRef]
	baseTip, ok2 := repo.Refs["refs/heads/"+pull.BaseRef]
	if !ok1 || !ok2 {
		return ""
	}
	if s.mergeableLocked(headTip, baseTip) != "MERGEABLE" {
		return ""
	}
	headObject, ok := s.objects[headTip]
	if !ok || headObject.Type != objCommit {
		return ""
	}
	headTree := parseCommit(headObject.Data).Tree
	commitData := buildCommit(headTree, []string{baseTip}, pull.Title+"\n", s.ident(), s.ident(), s.clock.next())
	mergeOID := s.putObjectLocked(objCommit, commitData)
	s.setRefLocked(repo, "refs/heads/"+pull.BaseRef, mergeOID)
	return mergeOID
}

// squashMerge is the locking wrapper (used by the REST merge handler).
func (s *Store) squashMerge(repo *Repo, pull *Pull) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.squashMergeLocked(repo, pull)
}

// ------------------------------------------------------------------ packs

// fetchPack builds a pack for a fetch, from the cache when the same ask
// was made before.
func (s *Store) fetchPack(repo *Repo, wants, haves []string, depth int, hasDepth bool, shallow []string) (pack []byte, shallowOut, unshallowOut []string) {
	sortedCopy := func(in []string) []string {
		out := make([]string, len(in))
		copy(out, in)
		sort.Strings(out)
		return out
	}
	depthStr := ""
	if hasDepth {
		depthStr = itoa(depth)
	}
	key := itoa(repo.ID) + "|" + strings.Join(sortedCopy(wants), ",") + "|" + strings.Join(sortedCopy(haves), ",") + "|" + depthStr + "|" + strings.Join(sortedCopy(shallow), ",")

	s.mu.RLock()
	cached, hit := s.packCache[key]
	s.mu.RUnlock()
	if hit {
		shallowOut, unshallowOut = s.shallowInfo(wants, depth, hasDepth, shallow)
		return cached, shallowOut, unshallowOut
	}

	// Singleflight: without this, N concurrent clones of the same repo
	// all miss the cache and all pay the full reachability walk + sort
	// + deflate. The first builder fills the cache; the rest re-check
	// it and return immediately.
	s.packMu.Lock()
	defer s.packMu.Unlock()
	s.mu.RLock()
	cached, hit = s.packCache[key]
	s.mu.RUnlock()
	if hit {
		shallowOut, unshallowOut = s.shallowInfo(wants, depth, hasDepth, shallow)
		return cached, shallowOut, unshallowOut
	}

	s.mu.RLock()
	var stopAt map[string]bool
	if len(shallow) > 0 {
		stopAt = map[string]bool{}
		for _, oid := range shallow {
			stopAt[oid] = true
		}
	}
	var exclude map[string]bool
	if len(haves) > 0 {
		exclude = s.reachableLocked(haves, reachOpts{stopAt: stopAt})
	}
	wanted := s.reachableLocked(wants, reachOpts{depth: depth, hasDepth: hasDepth, exclude: exclude})
	type rankedObj struct {
		obj GitObject
		oid string
	}
	ranked := make([]rankedObj, 0, len(wanted))
	for oid := range wanted {
		if object, ok := s.objects[oid]; ok {
			ranked = append(ranked, rankedObj{object, oid})
		}
	}
	s.mu.RUnlock()

	// Deterministic pack order: commits first, then trees, then tags,
	// then blobs, each by oid.
	rank := func(typ ObjType) int {
		switch typ {
		case objCommit:
			return 0
		case objTree:
			return 1
		case objTag:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		ri, rj := rank(ranked[i].obj.Type), rank(ranked[j].obj.Type)
		if ri != rj {
			return ri < rj
		}
		return ranked[i].oid < ranked[j].oid
	})
	objects := make([]GitObject, 0, len(ranked))
	for _, r := range ranked {
		objects = append(objects, r.obj)
	}
	pack = writePack(objects)
	shallowOut, unshallowOut = s.shallowInfo(wants, depth, hasDepth, shallow)

	s.mu.Lock()
	if len(s.packCache) >= 256 {
		s.packCache = map[string][]byte{}
	}
	s.packCache[key] = pack
	s.mu.Unlock()
	return pack, shallowOut, unshallowOut
}

// shallowInfo returns the shallow-info lines for a fetch: new
// boundaries, and old ones now deepened.
func (s *Store) shallowInfo(wants []string, depth int, hasDepth bool, shallow []string) (shallowOut, unshallowOut []string) {
	if !hasDepth {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	shallowOut = s.shallowBoundaryLocked(wants, depth)
	for _, boundary := range shallow {
		found := false
		for _, out := range shallowOut {
			if out == boundary {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if d := s.commitDepthFromLocked(wants, boundary); d >= 0 && d < depth-1 {
			unshallowOut = append(unshallowOut, boundary)
		}
	}
	return shallowOut, unshallowOut
}

// commitDepthFromLocked returns generations from any of roots down to
// target, or -1 when unreachable (caller holds a lock).
func (s *Store) commitDepthFromLocked(roots []string, target string) int {
	seen := map[string]bool{}
	type item struct {
		oid string
		d   int
	}
	queue := make([]item, 0, len(roots))
	for _, oid := range roots {
		queue = append(queue, item{oid, 0})
	}
	head := 0
	for head < len(queue) {
		cur := queue[head]
		head++
		if seen[cur.oid] {
			continue
		}
		seen[cur.oid] = true
		if cur.oid == target {
			return cur.d
		}
		object, ok := s.objects[cur.oid]
		if !ok || object.Type != objCommit {
			continue
		}
		for _, parent := range parseCommit(object.Data).Parents {
			queue = append(queue, item{parent, cur.d + 1})
		}
	}
	return -1
}

// ------------------------------------------------------------------ ls-refs

// lsRefs returns the ref rows for ls-refs/advertisements, cached per
// ref version.
func (s *Store) lsRefs(repo *Repo) []LsRefRow {
	s.mu.RLock()
	if repo.LsRefsCache != nil {
		cached := repo.LsRefsCache
		s.mu.RUnlock()
		return cached
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if repo.LsRefsCache != nil {
		return repo.LsRefsCache
	}
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]LsRefRow, 0, len(names))
	for _, name := range names {
		oid := repo.Refs[name]
		row := LsRefRow{OID: oid, Name: name}
		if strings.HasPrefix(name, "refs/tags/") {
			if object, ok := s.objects[oid]; ok && object.Type == objTag {
				target := parseTag(object.Data).Object
				if targetObj, ok := s.objects[target]; ok && targetObj.Type == objCommit {
					row.Peeled = target
				}
			}
		}
		rows = append(rows, row)
	}
	repo.LsRefsCache = rows
	return rows
}

// splitNonEmpty splits on sep and drops empty segments.
func splitNonEmpty(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
