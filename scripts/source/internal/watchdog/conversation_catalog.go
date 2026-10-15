package watchdog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ConversationCatalog discovers persisted Codex rollout manifests under the
// session roots the watchdog already monitors and lists them with opaque
// (root_id, thread_id) identities. It is a read-only view: it opens its own
// file handles and never touches RuntimeState file cursors or recovery
// decisions.

// conversationFilter selects which discovered conversations a list returns.
// The archive state is a separate axis from the record classification, so the
// filters partition the manifest set as ordinary(!archived), subagent
// (!archived), archived (any kind), or all.
type conversationFilter string

const (
	conversationFilterAll      conversationFilter = "all"
	conversationFilterOrdinary conversationFilter = "ordinary"
	conversationFilterSubagent conversationFilter = "subagent"
	conversationFilterArchived conversationFilter = "archived"
)

const (
	conversationKindOrdinary = "ordinary"
	conversationKindSubagent = "subagent"
)

var (
	errConversationFilterUnknown = errors.New("unknown conversation filter")
	errConversationCursorInvalid = errors.New("invalid conversation cursor")
	errConversationNotFound      = errors.New("conversation not found")
)

// Scan bounds keep a list call cheap and bounded regardless of how much data
// the rollout files hold. Manifest identity lives in the head of a file and
// the latest activity lives at its tail, so both windows are bounded instead
// of reading arbitrary middle content.
var (
	maxCatalogHeadBytes    = int64(512 * 1024)
	maxCatalogHeadLines    = 64
	maxCatalogTailBytes    = int64(512 * 1024)
	maxCatalogLinePrefix   = 4096
	maxCatalogScanFiles    = 4096
	maxCatalogScanDuration = 4 * time.Second

	// maxCatalogResolveCandidates bounds how many files may carry one thread
	// id inside a single root before resolve stops looking.
	maxCatalogResolveCandidates = 64

	defaultConversationPageLimit = 50
	maxConversationPageLimit     = 200
)

// parseConversationFilter validates a client-supplied filter name. An empty
// value selects the default ordinary view; "normal" is accepted as a legacy
// alias for the same view.
func parseConversationFilter(raw string) (conversationFilter, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "normal":
		return conversationFilterOrdinary, nil
	case string(conversationFilterAll):
		return conversationFilterAll, nil
	case string(conversationFilterOrdinary):
		return conversationFilterOrdinary, nil
	case string(conversationFilterSubagent):
		return conversationFilterSubagent, nil
	case string(conversationFilterArchived):
		return conversationFilterArchived, nil
	default:
		return "", fmt.Errorf("%w", errConversationFilterUnknown)
	}
}

// conversationSummary is one manifest entry handed to the API layer. It never
// carries a filesystem path; identity is (root_id, thread_id) only. It is an
// internal handoff type, not a wire shape — the API maps it onto
// conversationsListItem (api_conversations.go) for serialization.
type conversationSummary struct {
	RootID     string
	ThreadID   string
	Kind       string
	Archived   bool
	Title      string
	StartedAt  string
	LastActive string
}

// conversationListPage is one page of catalog output. NextCursor is opaque;
// callers must treat it as an unforgeable position token. Partial reports that
// scan bounds stopped the enumeration early. Like conversationSummary this is
// an internal handoff type; conversationsListResponse is the wire shape.
type conversationListPage struct {
	Items      []conversationSummary
	NextCursor string
	Partial    bool
}

type conversationCatalog struct {
	roots []sessionRoot
	byID  map[string]sessionRoot
}

func newConversationCatalog(roots []sessionRoot) *conversationCatalog {
	c := &conversationCatalog{byID: make(map[string]sessionRoot, len(roots))}
	for _, root := range roots {
		if strings.TrimSpace(root.CodexHome) == "" {
			continue
		}
		id := conversationRootID(root)
		if _, exists := c.byID[id]; exists {
			continue
		}
		c.roots = append(c.roots, root)
		c.byID[id] = root
	}
	return c
}

// conversationRootID derives the opaque identity of a whitelisted root from
// the canonicalized home path alone. It survives discovery ordering changes
// and exposes nothing about the location on disk. If the configured path text
// itself changes (user reconfigures a root), the id changes with it, which is
// correct: it is a different whitelist entry.
func conversationRootID(root sessionRoot) string {
	key := strings.ToLower(filepath.Clean(strings.TrimSpace(root.CodexHome)))
	sum := sha256.Sum256([]byte(key))
	return "r" + hex.EncodeToString(sum[:8])
}

// conversationManifest is the per-file inspection result before merging.
type conversationManifest struct {
	rootID     string
	threadID   string
	path       string
	archived   bool
	kind       string
	title      string
	startedAt  time.Time
	lastActive time.Time
}

// resolvedConversation is the whitelisted-file handoff used by
// ConversationReader: an opaque (root_id, thread_id) mapped back to a
// validated real path under the root that produced it.
type resolvedConversation struct {
	RootID   string
	ThreadID string
	Root     sessionRoot
	Path     string
	Archived bool
	Kind     string
}

func (c *conversationCatalog) list(ctx context.Context, filter conversationFilter, cursor string, limit int) (conversationListPage, error) {
	after, err := decodeConversationCursor(cursor)
	if err != nil {
		return conversationListPage{}, err
	}
	switch filter {
	case conversationFilterAll, conversationFilterOrdinary, conversationFilterSubagent, conversationFilterArchived:
	default:
		return conversationListPage{}, fmt.Errorf("%w", errConversationFilterUnknown)
	}
	if after != nil && after.Filter != string(filter) {
		return conversationListPage{}, fmt.Errorf("%w: filter changed between pages", errConversationCursorInvalid)
	}
	if limit <= 0 {
		limit = defaultConversationPageLimit
	}
	if limit > maxConversationPageLimit {
		limit = maxConversationPageLimit
	}

	manifests, partial, err := c.scanManifests(ctx, time.Now().Add(maxCatalogScanDuration))
	if err != nil {
		return conversationListPage{}, err
	}
	merged := mergeConversationManifests(manifests)
	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].lastActive.Equal(merged[j].lastActive) {
			return merged[i].lastActive.After(merged[j].lastActive)
		}
		if merged[i].rootID != merged[j].rootID {
			return merged[i].rootID < merged[j].rootID
		}
		return merged[i].threadID < merged[j].threadID
	})

	items := make([]conversationSummary, 0, limit)
	var last conversationManifest
	remaining := 0
	for _, manifest := range merged {
		if !filterMatches(filter, manifest) {
			continue
		}
		if after != nil && !manifestAfterCursor(manifest, *after) {
			continue
		}
		if len(items) >= limit {
			remaining++
			continue
		}
		items = append(items, manifest.toSummary())
		last = manifest
	}
	page := conversationListPage{Items: items, Partial: partial}
	if remaining > 0 {
		page.NextCursor = encodeConversationCursor(conversationCursor{
			Filter: string(filter),
			Last:   last.lastActive.UTC().Format(time.RFC3339Nano),
			Root:   last.rootID,
			Thread: last.threadID,
		})
	}
	return page, nil
}

func filterMatches(filter conversationFilter, m conversationManifest) bool {
	switch filter {
	case conversationFilterAll:
		return true
	case conversationFilterArchived:
		return m.archived
	case conversationFilterSubagent:
		return !m.archived && m.kind == conversationKindSubagent
	default:
		return !m.archived && m.kind == conversationKindOrdinary
	}
}

func manifestAfterCursor(m conversationManifest, c conversationCursor) bool {
	ts, err := time.Parse(time.RFC3339Nano, c.Last)
	if err != nil {
		return false
	}
	if !m.lastActive.Equal(ts) {
		return m.lastActive.Before(ts)
	}
	if m.rootID != c.Root {
		return m.rootID > c.Root
	}
	return m.threadID > c.Thread
}

func (s conversationManifest) toSummary() conversationSummary {
	summary := conversationSummary{
		RootID:   s.rootID,
		ThreadID: s.threadID,
		Kind:     s.kind,
		Archived: s.archived,
		Title:    s.title,
	}
	if !s.startedAt.IsZero() {
		summary.StartedAt = s.startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !s.lastActive.IsZero() {
		summary.LastActive = s.lastActive.UTC().Format(time.RFC3339Nano)
	}
	return summary
}

// conversationCursor is a keyset position: the (activity, root, thread) of the
// last emitted item. It encodes the filter so a cursor cannot be replayed
// against a different view.
type conversationCursor struct {
	V      int    `json:"v"`
	Filter string `json:"f"`
	Last   string `json:"ts"`
	Root   string `json:"r"`
	Thread string `json:"t"`
}

func encodeConversationCursor(c conversationCursor) string {
	c.V = 1
	data, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeConversationCursor(raw string) (*conversationCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	var cursor conversationCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	// A zero timestamp is a legitimate position: manifests with no persisted
	// record timestamps sort last, so the cursor must be able to sit among them.
	_, tsErr := time.Parse(time.RFC3339Nano, cursor.Last)
	if cursor.V != 1 || tsErr != nil ||
		threadIDFromPath(cursor.Thread+".jsonl") != strings.ToLower(cursor.Thread) ||
		strings.TrimSpace(cursor.Root) == "" {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	return &cursor, nil
}

func (c *conversationCatalog) scanManifests(ctx context.Context, deadline time.Time) ([]conversationManifest, bool, error) {
	var out []conversationManifest
	filesSeen := 0
	partial := false
	for _, root := range c.roots {
		rootID := conversationRootID(root)
		for _, directory := range root.scanDirectories() {
			archived := rootDirIsArchived(root, directory)
			realDir, err := realCatalogDir(directory)
			if err != nil {
				continue
			}
			walkErr := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if time.Now().After(deadline) {
					partial = true
					return errCatalogScanTruncated
				}
				if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
					return nil
				}
				threadID := threadIDFromPath(path)
				if threadID == "" {
					return nil
				}
				if filesSeen >= maxCatalogScanFiles {
					partial = true
					return errCatalogScanTruncated
				}
				filesSeen++
				resolved, ok := catalogCandidatePath(realDir, path)
				if !ok {
					return nil
				}
				manifest, ok := inspectConversationManifest(resolved, threadID)
				if !ok {
					return nil
				}
				manifest.rootID = rootID
				manifest.archived = archived
				out = append(out, manifest)
				return nil
			})
			if errors.Is(walkErr, errCatalogScanTruncated) {
				return out, true, nil
			}
			if walkErr != nil {
				return out, partial, walkErr
			}
		}
	}
	return out, partial, ctx.Err()
}

var errCatalogScanTruncated = errors.New("catalog scan truncated")

// mergeConversationManifests collapses multiple rollout files that carry the
// same thread id under one whitelisted root (archived copies, resumed
// variants) into a single conversation identity. A live sessions/ copy wins
// over an archived_sessions/ copy; the activity window is the union.
func mergeConversationManifests(manifests []conversationManifest) []conversationManifest {
	byKey := make(map[string]int, len(manifests))
	merged := make([]conversationManifest, 0, len(manifests))
	for _, m := range manifests {
		key := m.rootID + "\x00" + m.threadID
		index, exists := byKey[key]
		if !exists {
			byKey[key] = len(merged)
			merged = append(merged, m)
			continue
		}
		current := merged[index]
		if m.lastActive.After(current.lastActive) {
			current.lastActive = m.lastActive
		}
		if !m.startedAt.IsZero() && (current.startedAt.IsZero() || m.startedAt.Before(current.startedAt)) {
			current.startedAt = m.startedAt
		}
		if preferConversationManifest(m, current) {
			current.path = m.path
			current.archived = m.archived
			current.kind = m.kind
			if m.title != "" {
				current.title = m.title
			}
		} else if current.title == "" {
			current.title = m.title
		}
		merged[index] = current
	}
	return merged
}

func preferConversationManifest(candidate, current conversationManifest) bool {
	if candidate.archived != current.archived {
		return !candidate.archived
	}
	if !candidate.lastActive.Equal(current.lastActive) {
		return candidate.lastActive.After(current.lastActive)
	}
	return candidate.path < current.path
}

// catalogSessionMeta captures the identity fields of a session_meta record.
// Message content is never decoded here.
type catalogSessionMeta struct {
	ID             string          `json:"id"`
	Source         json.RawMessage `json:"source"`
	ParentThreadID string          `json:"parent_thread_id"`
}

// inspectConversationManifest reads the bounded head and tail windows of one
// rollout file. A file is only a manifest when it carries a session_meta
// record whose id matches the thread id in its filename; anything else is
// skipped so a renamed or foreign file can never borrow another identity.
// File mtime and rename time are deliberately never consulted: only persisted
// record timestamps feed the activity ordering.
func inspectConversationManifest(path, threadID string) (conversationManifest, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return conversationManifest{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return conversationManifest{}, false
	}
	defer file.Close()
	return inspectConversationManifestHandle(file, info, path, threadID)
}

// inspectConversationManifestHandle is the open-handle form of
// inspectConversationManifest: identity is validated against the very file
// the caller opened, so a path swap between resolve and read can never let a
// foreign file borrow a resolved identity (TOCTOU). info must be a Stat of
// file, and path is only used for the returned manifest's identity fields.
func inspectConversationManifestHandle(file *os.File, info os.FileInfo, path, threadID string) (conversationManifest, bool) {
	manifest := conversationManifest{path: filepath.Clean(path), threadID: threadID}
	headLen := info.Size()
	if headLen > maxCatalogHeadBytes {
		headLen = maxCatalogHeadBytes
	}
	head := make([]byte, headLen)
	read, err := file.ReadAt(head, 0)
	if err != nil && read == 0 {
		return conversationManifest{}, false
	}
	head = head[:read]

	var meta *catalogSessionMeta
	lines := bytes.Split(head, []byte{'\n'})
	complete := len(lines)
	if read > 0 && head[read-1] != '\n' {
		// A line without a terminating newline is either window-cut or a
		// partial append; it is not a persisted record yet.
		complete--
	}
	if complete > maxCatalogHeadLines {
		complete = maxCatalogHeadLines
	}
	for _, raw := range lines[:complete] {
		line := bytes.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		if ts, ok := rolloutLineTimestamp(line); ok {
			if manifest.startedAt.IsZero() || ts.Before(manifest.startedAt) {
				manifest.startedAt = ts
			}
			if ts.After(manifest.lastActive) {
				manifest.lastActive = ts
			}
		}
		if meta == nil && bytes.Contains(line, []byte(`"session_meta"`)) {
			if parsed, ok := parseCatalogSessionMeta(line); ok {
				meta = parsed
			}
		}
		if manifest.title == "" {
			manifest.title = titleFromRolloutLine(line)
		}
	}
	if info.Size() > int64(read) {
		if ts, ok := tailTimestamp(file, info.Size()); ok && ts.After(manifest.lastActive) {
			manifest.lastActive = ts
		}
	}
	if meta == nil || strings.ToLower(strings.TrimSpace(meta.ID)) != threadID {
		return conversationManifest{}, false
	}
	manifest.kind = conversationKindOrdinary
	if catalogMetaIsSubagent(*meta) {
		manifest.kind = conversationKindSubagent
	}
	return manifest, true
}

func parseCatalogSessionMeta(line []byte) (*catalogSessionMeta, bool) {
	var envelope eventEnvelope
	if json.Unmarshal(line, &envelope) != nil || envelope.Type != "session_meta" {
		return nil, false
	}
	var meta catalogSessionMeta
	if json.Unmarshal(envelope.Payload, &meta) != nil {
		return nil, false
	}
	return &meta, true
}

// catalogMetaIsSubagent classifies the persisted source marker. Upstream
// writes subagent provenance as {"subagent": ...} or {"subAgent": ...} inside
// session_meta.source, and thread-spawned children may also carry
// parent_thread_id on the payload.
func catalogMetaIsSubagent(meta catalogSessionMeta) bool {
	if threadIDFromPath(strings.ToLower(strings.TrimSpace(meta.ParentThreadID))+".jsonl") != "" {
		return true
	}
	raw := bytes.TrimSpace(meta.Source)
	if len(raw) == 0 {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return normalizedSourceKey(text) == "subagent"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for key := range object {
			if normalizedSourceKey(key) == "subagent" {
				return true
			}
		}
	}
	return false
}

func normalizedSourceKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(key), "_", ""), "-", "")
}

// rolloutLineTimestamp extracts only the envelope timestamp without decoding
// the record payload.
func rolloutLineTimestamp(line []byte) (time.Time, bool) {
	prefix := line
	if len(prefix) > maxCatalogLinePrefix {
		prefix = prefix[:maxCatalogLinePrefix]
	}
	value, ok := topLevelStringField(prefix, "timestamp")
	if !ok {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// tailTimestamp finds the newest persisted record timestamp in the bounded
// tail window of the file.
func tailTimestamp(file *os.File, size int64) (time.Time, bool) {
	window := size
	if window > maxCatalogTailBytes {
		window = maxCatalogTailBytes
	}
	start := size - window
	buffer := make([]byte, window)
	read, err := file.ReadAt(buffer, start)
	if err != nil && read == 0 {
		return time.Time{}, false
	}
	tail := buffer[:read]
	var latest time.Time
	lines := bytes.Split(tail, []byte{'\n'})
	for i, raw := range lines {
		if start > 0 && i == 0 {
			continue // window cut the first line mid-record
		}
		if i == len(lines)-1 && read > 0 && tail[read-1] != '\n' {
			continue // partial final append waits for completion
		}
		line := bytes.TrimRight(raw, "\r")
		if len(line) == 0 {
			continue
		}
		if ts, ok := rolloutLineTimestamp(line); ok && ts.After(latest) {
			latest = ts
		}
	}
	return latest, !latest.IsZero()
}

// resolve maps an opaque (root_id, thread_id) pair back to the validated
// rollout file a reader may open. It never accepts client-supplied paths.
func (c *conversationCatalog) resolve(rootID, threadID string) (resolvedConversation, error) {
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	if threadIDFromPath(threadID+".jsonl") != threadID {
		return resolvedConversation{}, fmt.Errorf("%w: invalid thread id", errConversationNotFound)
	}
	root, ok := c.byID[rootID]
	if !ok {
		return resolvedConversation{}, fmt.Errorf("%w: unknown root", errConversationNotFound)
	}
	var candidates []conversationManifest
	for _, directory := range root.scanDirectories() {
		archived := rootDirIsArchived(root, directory)
		realDir, err := realCatalogDir(directory)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || threadIDFromPath(path) != threadID {
				return nil
			}
			if len(candidates) >= maxCatalogResolveCandidates {
				return errCatalogScanTruncated
			}
			resolved, ok := catalogCandidatePath(realDir, path)
			if !ok {
				return nil
			}
			manifest, ok := inspectConversationManifest(resolved, threadID)
			if !ok {
				return nil
			}
			manifest.rootID = rootID
			manifest.archived = archived
			candidates = append(candidates, manifest)
			return nil
		})
	}
	if len(candidates) == 0 {
		return resolvedConversation{}, fmt.Errorf("%w", errConversationNotFound)
	}
	primary := candidates[0]
	for _, candidate := range candidates[1:] {
		if preferConversationManifest(candidate, primary) {
			primary = candidate
		}
	}
	return resolvedConversation{
		RootID:   rootID,
		ThreadID: threadID,
		Root:     root,
		Path:     primary.path,
		Archived: primary.archived,
		Kind:     primary.kind,
	}, nil
}

func rootDirIsArchived(root sessionRoot, directory string) bool {
	if strings.TrimSpace(root.ArchivedSessions) == "" {
		return false
	}
	if !strings.EqualFold(filepath.Clean(directory), filepath.Clean(root.ArchivedSessions)) {
		return false
	}
	// A root configured with sessions == archived_sessions scans as live.
	return !strings.EqualFold(filepath.Clean(directory), filepath.Clean(root.Sessions))
}

// realCatalogDir resolves the canonical form of a whitelisted scan directory
// so symlinked homes compare correctly against candidates.
func realCatalogDir(directory string) (string, error) {
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// catalogCandidatePath resolves a manifest candidate to its real path and
// verifies it stays inside the whitelisted directory. This rejects
// traversal, symlink, and junction escapes before the file is ever opened.
func catalogCandidatePath(realDir, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", false
	}
	resolved = filepath.Clean(abs)
	if !catalogPathWithin(realDir, resolved) {
		return "", false
	}
	return resolved, true
}

// catalogPathWithin reports whether path is inside root after canonical
// cleanup. Windows compares case-insensitively; other platforms keep the
// filesystem's own case sensitivity so a differently-cased sibling directory
// cannot pass the prefix check.
func catalogPathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if conversationPathCaseInsensitive {
		root = strings.ToLower(root)
		path = strings.ToLower(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
