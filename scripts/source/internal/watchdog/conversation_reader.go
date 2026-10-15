package watchdog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// ConversationReader projects persisted rollout records into safe display
// items. It maps (root_id, thread_id) back to a whitelisted file through the
// catalog, then reads that file with its own handles: it never shares
// recovery scanner offsets, never touches RuntimeState.Files, and never calls
// app-server conversation APIs or process memory. Only complete, newline-
// terminated records are projected; a partial final append waits until it is
// persisted, and streaming deltas that never reached disk are never promised.

// Item kinds emitted by the projection.
const (
	conversationItemUserMessage      = "user_message"
	conversationItemAssistantMessage = "assistant_message"
	conversationItemToolCall         = "tool_call"
	conversationItemToolResult       = "tool_result"
	conversationItemBoundary         = "boundary"
	conversationItemUnknown          = "unknown"
)

// Boundary markers for recognized lifecycle/boundary records. There is
// deliberately no "archived" marker: archiving is a file move that writes no
// rollout record, so the page-level Archived flag (plus the UI banner it
// drives) already covers it.
const (
	conversationBoundarySession       = "session"
	conversationBoundarySubagent      = "subagent"
	conversationBoundaryFork          = "fork"
	conversationBoundaryCompaction    = "compaction"
	conversationBoundaryTurnStarted   = "turn_started"
	conversationBoundaryTurnComplete  = "turn_complete"
	conversationBoundaryTurnAborted   = "turn_aborted"
	conversationBoundaryThreadRenamed = "thread_renamed"
	conversationBoundaryGoalUpdated   = "goal_updated"
	conversationBoundaryRollback      = "rollback"
	conversationBoundaryError         = "error"
	conversationBoundaryWarning       = "warning"
)

// Record families a projected item can come from. Rollouts persist one
// logical chat message twice — once as an event_msg
// user_message/agent_message and once as a response_item message — so the
// family tag feeds the projection's duplicate collapse
// (dedupeConversationMessages).
const (
	conversationFamilyEventMessage = "event_msg"
	conversationFamilyResponseItem = "response_item"
)

// conversationMessageDedupSpan bounds the duplicate-collapse window in
// emitted items. The two records of one logical message are persisted within
// a few lines of each other; two genuinely repeated identical messages — a
// user sending "ok" twice — survive because they land further apart or arrive
// from the same family.
const conversationMessageDedupSpan = 8

// Completeness states for a page of items.
const (
	conversationStateCompletePersisted = "complete_persisted"
	conversationStatePartial           = "partial"
	conversationStateUnsupported       = "unsupported"
)

// Read directions. Backward pages the persisted history newest-first for the
// chat view; forward follows new appended records incrementally.
const (
	conversationDirectionBackward = "backward"
	conversationDirectionForward  = "forward"
)

var (
	errConversationFileChanged      = errors.New("conversation file changed")
	errConversationDirectionUnknown = errors.New("unknown conversation direction")
)

// Reader bounds: one persisted line can hold a large tool result, so the line
// cap is generous but bounded; per-call byte and time budgets keep a detail
// read cheap regardless of file size.
var (
	maxReaderLineBytes    = int64(1024 * 1024)
	maxReaderScanBytes    = int64(4 * 1024 * 1024)
	maxReaderScanDuration = 4 * time.Second
	maxReaderBlockBytes   = int64(64 * 1024)
	maxItemTextRunes      = 4096
	maxMarkerRunes        = 64

	defaultReaderPageLimit = 50
	maxReaderPageLimit     = 200
)

// conversationItem is a projected display item. It carries only content that
// was persisted in the rollout file; Omitted marks parts deliberately dropped
// (attachments, oversized records, truncation).
type conversationItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Marker    string `json:"marker,omitempty"`
	Collapsed bool   `json:"collapsed,omitempty"`
	Omitted   bool   `json:"omitted,omitempty"`
	// family records which persisted record family produced the item
	// (conversationFamily*); it feeds message dedup and is internal-only.
	family string
}

// conversationItemPage is one page of projected items in chronological order.
// NextCursor continues in the requested direction; TailCursor is a stable
// anchor at the end of persisted content so a client can poll forward for
// newly appended records. State reports how complete the projection is.
type conversationItemPage struct {
	Items      []conversationItem `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
	TailCursor string             `json:"tail_cursor,omitempty"`
	State      string             `json:"state"`
	Archived   bool               `json:"archived,omitempty"`
	Kind       string             `json:"kind,omitempty"`
}

type conversationReader struct {
	catalog *conversationCatalog
}

func newConversationReader(catalog *conversationCatalog) *conversationReader {
	return &conversationReader{catalog: catalog}
}

// readerCursor is the opaque position token for item pages. The fingerprint
// (first-line hash, size, mtime at issue) detects truncation, replacement,
// and rotation without trusting client-supplied positions.
type readerCursor struct {
	V    int    `json:"v"`
	Head string `json:"h"`
	Off  int64  `json:"o"`
	Size int64  `json:"s"`
	Mod  int64  `json:"m"`
}

func encodeReaderCursor(head string, off, size, mod int64) string {
	data, err := json.Marshal(readerCursor{V: 1, Head: head, Off: off, Size: size, Mod: mod})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeReaderCursor(raw string) (*readerCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	var cursor readerCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	head, err := hex.DecodeString(cursor.Head)
	if cursor.V != 1 || err != nil || len(head) != sha256.Size ||
		cursor.Off < 0 || cursor.Size < 0 || cursor.Mod < 0 {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	return &cursor, nil
}

func parseConversationDirection(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", conversationDirectionBackward:
		return conversationDirectionBackward, nil
	case conversationDirectionForward:
		return conversationDirectionForward, nil
	default:
		return "", fmt.Errorf("%w", errConversationDirectionUnknown)
	}
}

// readItems resolves the opaque identity to a whitelisted rollout file, then
// returns one bounded page of projected items. The file is re-validated after
// open so a swapped file can never borrow the resolved identity (TOCTOU).
func (r *conversationReader) readItems(ctx context.Context, rootID, threadID, cursor, direction string, limit int) (conversationItemPage, error) {
	direction, err := parseConversationDirection(direction)
	if err != nil {
		return conversationItemPage{}, err
	}
	resolved, err := r.catalog.resolve(rootID, threadID)
	if err != nil {
		return conversationItemPage{}, err
	}
	return r.readResolvedItems(ctx, resolved, cursor, direction, limit)
}

// readResolvedItems reads one page from an already-whitelisted rollout file.
// Splitting it from resolve keeps the identity mapping the only place that
// accepts (root_id, thread_id).
func (r *conversationReader) readResolvedItems(ctx context.Context, resolved resolvedConversation, cursor, direction string, limit int) (conversationItemPage, error) {
	threadID := resolved.ThreadID
	position, err := decodeReaderCursor(cursor)
	if err != nil {
		return conversationItemPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return conversationItemPage{}, err
	}
	if limit <= 0 {
		limit = defaultReaderPageLimit
	}
	if limit > maxReaderPageLimit {
		limit = maxReaderPageLimit
	}

	file, err := os.Open(resolved.Path)
	if err != nil {
		return conversationItemPage{}, fmt.Errorf("%w: rollout open", errConversationNotFound)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return conversationItemPage{}, fmt.Errorf("%w: rollout stat", errConversationNotFound)
	}
	size := info.Size()
	mod := info.ModTime().UnixNano()

	page := conversationItemPage{
		Items:    []conversationItem{},
		State:    conversationStateCompletePersisted,
		Archived: resolved.Archived,
		Kind:     resolved.Kind,
	}
	// Identity is only trustworthy if the file still carries a session_meta
	// for this thread. The check runs on the already-open handle — never a
	// second path open — so a swapped file cannot borrow the resolved
	// identity between resolve and read. Anything else is an unsupported
	// projection, not an empty conversation.
	if _, ok := inspectConversationManifestHandle(file, info, resolved.Path, threadID); !ok {
		page.State = conversationStateUnsupported
		return page, nil
	}
	head, ok := readFirstRolloutLine(file, size)
	if !ok {
		page.State = conversationStateUnsupported
		return page, nil
	}
	headSum := sha256.Sum256(head)
	headHex := hex.EncodeToString(headSum[:])
	if position != nil {
		if size < position.Size || headHex != position.Head ||
			mod < position.Mod || position.Off > size {
			return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
		}
		if position.Off > 0 {
			// Issued offsets only ever sit on record boundaries: byte Off-1
			// must be the newline terminating the previous record.
			boundary := make([]byte, 1)
			if _, err := file.ReadAt(boundary, position.Off-1); err != nil || boundary[0] != '\n' {
				return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
			}
		}
	}

	tail := rolloutTailOffset(file, size)
	page.TailCursor = encodeReaderCursor(headHex, tail, size, mod)
	deadline := time.Now().Add(maxReaderScanDuration)

	if direction == conversationDirectionForward {
		start := int64(0)
		if position != nil {
			start = position.Off
		}
		items, resume, hasMore, partial, err := scanConversationForward(ctx, file, start, tail, limit, deadline)
		if err != nil {
			return conversationItemPage{}, err
		}
		page.Items = dedupeConversationMessages(items)
		if partial {
			page.State = conversationStatePartial
		}
		if hasMore {
			page.NextCursor = encodeReaderCursor(headHex, resume, size, mod)
		}
		return page, nil
	}

	end := tail
	if position != nil {
		if position.Off > tail {
			return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
		}
		end = position.Off
	}
	items, resume, hasMore, partial, err := scanConversationBackward(ctx, file, end, limit, deadline)
	if err != nil {
		return conversationItemPage{}, err
	}
	page.Items = dedupeConversationMessages(items)
	if partial {
		page.State = conversationStatePartial
	}
	if hasMore {
		page.NextCursor = encodeReaderCursor(headHex, resume, size, mod)
	}
	return page, nil
}

// readFirstRolloutLine returns the first complete (newline-terminated) line
// bounded by the line cap. It feeds the identity fingerprint.
func readFirstRolloutLine(file *os.File, size int64) ([]byte, bool) {
	if size <= 0 {
		return nil, false
	}
	window := size
	if window > maxReaderLineBytes {
		window = maxReaderLineBytes
	}
	buffer := make([]byte, window)
	read, err := file.ReadAt(buffer, 0)
	if err != nil && read == 0 {
		return nil, false
	}
	buffer = buffer[:read]
	index := bytes.IndexByte(buffer, '\n')
	if index < 0 {
		return nil, false
	}
	return bytes.TrimRight(buffer[:index], "\r"), true
}

// rolloutTailOffset returns the byte offset where persisted content ends: the
// start of a trailing incomplete line, or the file size when the last record
// is newline-terminated. The backward search is bounded; a partial line
// larger than the window projects as an oversized placeholder either way, so
// falling back to the window edge stays safe.
func rolloutTailOffset(file *os.File, size int64) int64 {
	if size <= 0 {
		return 0
	}
	window := size
	if window > maxReaderLineBytes {
		window = maxReaderLineBytes
	}
	start := size - window
	buffer := make([]byte, window)
	read, err := file.ReadAt(buffer, start)
	if err != nil && read == 0 {
		return size
	}
	buffer = buffer[:read]
	if index := bytes.LastIndexByte(buffer, '\n'); index >= 0 {
		return start + int64(index) + 1
	}
	return start
}

// scanConversationBackward collects up to limit projected items scanning
// toward the head from end (exclusive). Items come out in chronological
// order. resume is the start offset of the oldest line this page processed:
// a continuing backward page ends there, so emitted content is never replayed
// and every issued offset sits after a newline (the cursor boundary
// invariant). hasMore reports whether unprocessed content remains before it.
func scanConversationBackward(ctx context.Context, file *os.File, end int64, limit int, deadline time.Time) (items []conversationItem, resume int64, hasMore, partial bool, err error) {
	collected := make([]conversationItem, 0, limit)
	lineEnd := end
	boundary := end
	pos := end
	scanned := int64(0)
	stopped := false
	for pos > 0 && !stopped {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, partial, err
		}
		remaining := maxReaderScanBytes - scanned
		if time.Now().After(deadline) || remaining <= 0 {
			partial = true
			break
		}
		block := pos
		if block > maxReaderBlockBytes {
			block = maxReaderBlockBytes
		}
		if block > remaining {
			block = remaining
		}
		start := pos - block
		buffer := make([]byte, block)
		read, readErr := file.ReadAt(buffer, start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, 0, false, partial, readErr
		}
		buffer = buffer[:read]
		scanned += int64(len(buffer))
		for i := len(buffer) - 1; i >= 0; i-- {
			if buffer[i] != '\n' {
				continue
			}
			lineStart := start + int64(i) + 1
			if lineStart < lineEnd {
				item, emit, degraded, itemErr := readConversationLine(file, lineStart, lineEnd)
				if itemErr != nil {
					return nil, 0, false, partial, itemErr
				}
				if degraded {
					partial = true
				}
				if emit {
					collected = append(collected, item)
				}
			}
			boundary = lineStart
			lineEnd = lineStart - 1
			if len(collected) >= limit {
				stopped = true
				break
			}
		}
		pos = start
	}
	if !stopped && pos == 0 && lineEnd > 0 {
		// The first line ends at lineEnd with no leading newline. Only process
		// it when the scan really reached the head; a bounds break leaves
		// lineEnd mid-file and that range is not one line.
		item, emit, degraded, itemErr := readConversationLine(file, 0, lineEnd)
		if itemErr != nil {
			return nil, 0, false, partial, itemErr
		}
		if degraded {
			partial = true
		}
		if emit {
			collected = append(collected, item)
		}
		boundary = 0
		lineEnd = 0
	}
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, boundary, boundary > 0, partial, nil
}

// scanConversationForward projects complete lines starting at start up to
// tail (the end of persisted content). resume is the byte offset of the first
// unread byte after the last processed line; hasMore reports whether more
// complete content sits between resume and tail.
func scanConversationForward(ctx context.Context, file *os.File, start, tail int64, limit int, deadline time.Time) (items []conversationItem, resume int64, hasMore, partial bool, err error) {
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, false, false, err
	}
	reader := bufio.NewReaderSize(file, int(maxReaderBlockBytes))
	pos := start
	lineStart := start
	scanned := int64(0)
	collected := make([]conversationItem, 0, limit)
	var line []byte
	oversized := false
	for pos < tail {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, partial, err
		}
		if time.Now().After(deadline) || scanned >= maxReaderScanBytes {
			return collected, pos, pos < tail, true, nil
		}
		fragment, readErr := reader.ReadSlice('\n')
		pos += int64(len(fragment))
		scanned += int64(len(fragment))
		if !oversized {
			line = append(line, fragment...)
			if int64(len(line)) > maxReaderLineBytes {
				// The line cap hit: stop buffering content but keep consuming
				// to the record boundary so following records still project.
				oversized = true
				line = nil
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return collected, pos, pos < tail, partial, readErr
		}
		complete := readErr == nil
		if oversized {
			if !complete {
				// An oversized record still being appended is not persisted;
				// resume from its start on the next poll.
				pos = lineStart
				break
			}
			partial = true
			collected = append(collected, conversationItem{
				ID:      conversationItemID(lineStart),
				Kind:    conversationItemUnknown,
				Marker:  "oversized",
				Omitted: true,
			})
		} else if complete {
			item, emit, degraded, itemErr := projectConversationLine(bytes.TrimSuffix(line, []byte{'\n'}), lineStart)
			if itemErr != nil {
				return collected, pos, pos < tail, partial, itemErr
			}
			if degraded {
				partial = true
			}
			if emit {
				collected = append(collected, item)
			}
		}
		line = line[:0]
		oversized = false
		if readErr == io.EOF {
			break
		}
		lineStart = pos
		if len(collected) >= limit {
			return collected, pos, pos < tail, partial, nil
		}
	}
	return collected, pos, pos < tail, partial, nil
}

// readConversationLine loads one line span and projects it. Lines past the
// size cap are never fully read; they become bounded placeholders.
func readConversationLine(file *os.File, start, end int64) (conversationItem, bool, bool, error) {
	length := end - start
	if length <= 0 {
		return conversationItem{}, false, false, nil
	}
	if length > maxReaderLineBytes {
		return conversationItem{
			ID:      conversationItemID(start),
			Kind:    conversationItemUnknown,
			Marker:  "oversized",
			Omitted: true,
		}, true, true, nil
	}
	buffer := make([]byte, length)
	if _, err := file.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
		return conversationItem{}, false, false, err
	}
	return projectConversationLine(buffer, start)
}

func conversationItemID(offset int64) string {
	return fmt.Sprintf("i%x", offset)
}

// projectConversationLine maps one persisted record to a display item. The
// emit result is false for recognized record families the chat view
// deliberately does not show (settings, quota, reasoning, streaming noise).
// degraded is true when the record could not be faithfully projected.
func projectConversationLine(line []byte, offset int64) (conversationItem, bool, bool, error) {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return conversationItem{}, false, false, nil
	}
	item := conversationItem{ID: conversationItemID(offset)}
	var envelope eventEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		item.Kind = conversationItemUnknown
		item.Marker = "unreadable"
		item.Omitted = true
		return item, true, true, nil
	}
	if ts, err := time.Parse(time.RFC3339Nano, envelope.Timestamp); err == nil {
		item.Timestamp = ts.UTC().Format(time.RFC3339Nano)
	}
	switch envelope.Type {
	case "session_meta":
		return projectSessionMeta(item, envelope.Payload), true, false, nil
	case "response_item":
		return projectResponseItem(item, envelope.Payload)
	case "event_msg":
		return projectEventMessage(item, envelope.Payload)
	case "compacted":
		var payload struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(envelope.Payload, &payload)
		text, truncated := clampItemText(payload.Message)
		item.Kind = conversationItemBoundary
		item.Marker = conversationBoundaryCompaction
		item.Text = text
		item.Collapsed = true
		item.Omitted = truncated
		return item, true, false, nil
	case "turn_context":
		// Turn settings are recognized but not part of the chat projection.
		return conversationItem{}, false, false, nil
	default:
		item.Kind = conversationItemUnknown
		item.Marker = sanitizeMarker(envelope.Type)
		return item, true, false, nil
	}
}

type conversationSessionMetaPayload struct {
	ID             string          `json:"id"`
	Source         json.RawMessage `json:"source"`
	ParentThreadID string          `json:"parent_thread_id"`
	ForkedFrom     string          `json:"forked_from"`
	Title          string          `json:"title"`
	ThreadName     string          `json:"thread_name"`
}

func projectSessionMeta(item conversationItem, raw json.RawMessage) conversationItem {
	var payload conversationSessionMetaPayload
	if json.Unmarshal(raw, &payload) != nil {
		item.Kind = conversationItemBoundary
		item.Marker = conversationBoundarySession
		return item
	}
	meta := catalogSessionMeta{ID: payload.ID, Source: payload.Source, ParentThreadID: payload.ParentThreadID}
	item.Kind = conversationItemBoundary
	item.Marker = conversationBoundarySession
	if catalogMetaIsSubagent(meta) {
		item.Marker = conversationBoundarySubagent
	} else if metaHasForkSource(payload) {
		item.Marker = conversationBoundaryFork
	}
	title := payload.Title
	if strings.TrimSpace(title) == "" {
		title = payload.ThreadName
	}
	item.Text, item.Omitted = clampItemText(title)
	return item
}

// metaHasForkSource recognizes persisted fork provenance on session_meta: an
// explicit forked_from pointer or a source object keyed as a fork.
func metaHasForkSource(payload conversationSessionMetaPayload) bool {
	if strings.TrimSpace(payload.ForkedFrom) != "" {
		return true
	}
	raw := bytes.TrimSpace(payload.Source)
	if len(raw) == 0 {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return normalizedSourceKey(text) == "fork"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for key := range object {
			if normalizedSourceKey(key) == "fork" {
				return true
			}
		}
	}
	return false
}

// conversationItemPayload covers the response_item shapes the projection
// understands. Fields are deliberately narrow; unknown keys are ignored.
type conversationItemPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Action struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
	} `json:"action"`
	Query json.RawMessage `json:"query"`
}

func projectResponseItem(item conversationItem, raw json.RawMessage) (conversationItem, bool, bool, error) {
	var payload conversationItemPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		item.Kind = conversationItemUnknown
		item.Marker = "response_item"
		item.Omitted = true
		return item, true, true, nil
	}
	switch payload.Type {
	case "message":
		message, emit := projectMessageItem(item, payload)
		return message, emit, false, nil
	case "function_call", "custom_tool_call":
		text := rawToText(payload.Arguments)
		if payload.Type == "custom_tool_call" {
			text = rawToText(payload.Input)
		}
		summary, truncated := clampItemText(text)
		item.Kind = conversationItemToolCall
		item.Tool = sanitizeMarker(payload.Name)
		item.Text = summary
		item.Collapsed = true
		item.Omitted = truncated
		return item, true, false, nil
	case "function_call_output", "custom_tool_call_output":
		text, truncated := clampItemText(toolOutputText(payload.Output))
		item.Kind = conversationItemToolResult
		item.Text = text
		item.Collapsed = true
		item.Omitted = truncated
		return item, true, false, nil
	case "local_shell_call":
		summary, truncated := clampItemText(strings.Join(payload.Action.Command, " "))
		item.Kind = conversationItemToolCall
		item.Tool = "shell"
		item.Text = summary
		item.Collapsed = true
		item.Omitted = truncated
		return item, true, false, nil
	case "web_search_call":
		summary, truncated := clampItemText(rawToText(payload.Query))
		item.Kind = conversationItemToolCall
		item.Tool = "web_search"
		item.Text = summary
		item.Collapsed = true
		item.Omitted = truncated
		return item, true, false, nil
	case "reasoning":
		// Reasoning is a recognized family the v1 chat view does not render.
		return conversationItem{}, false, false, nil
	default:
		item.Kind = conversationItemUnknown
		item.Marker = sanitizeMarker(payload.Type)
		return item, true, false, nil
	}
}

// projectMessageItem projects response_item messages. Only real user and
// assistant roles render; developer/system records (including the watchdog's
// own recovery notice) never surface as conversation content.
func projectMessageItem(item conversationItem, payload conversationItemPayload) (conversationItem, bool) {
	var parts []string
	omitted := false
	for _, content := range payload.Content {
		switch content.Type {
		case "input_text", "output_text", "text":
			if text := strings.TrimRight(content.Text, "\r\n"); text != "" {
				parts = append(parts, text)
			}
		default:
			omitted = true
		}
	}
	if len(parts) == 0 && !omitted {
		return conversationItem{}, false
	}
	text, truncated := clampItemText(strings.Join(parts, "\n"))
	omitted = omitted || truncated
	switch payload.Role {
	case "user":
		item.Kind = conversationItemUserMessage
	case "assistant":
		item.Kind = conversationItemAssistantMessage
	default:
		return conversationItem{}, false
	}
	item.Text = text
	item.Omitted = omitted
	item.family = conversationFamilyResponseItem
	return item, true
}

// conversationEventPayload covers the event_msg shapes the projection shows.
// Unrecognized event payload types are skipped: the event stream carries
// streaming/diagnostic noise that must not fabricate chat items.
type conversationEventPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Name    string `json:"name"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Goal    *struct {
		Status string `json:"status"`
	} `json:"goal"`
	Images []json.RawMessage `json:"images"`
}

// eventBoundaryMarkers maps recognized event_msg payload types to boundary
// markers. Everything else in the event stream is skipped, never shown.
var eventBoundaryMarkers = map[string]string{
	"task_started":        conversationBoundaryTurnStarted,
	"task_complete":       conversationBoundaryTurnComplete,
	"turn_aborted":        conversationBoundaryTurnAborted,
	"thread_name_updated": conversationBoundaryThreadRenamed,
	"thread_goal_updated": conversationBoundaryGoalUpdated,
	"context_compacted":   conversationBoundaryCompaction,
	"thread_rolled_back":  conversationBoundaryRollback,
	"error":               conversationBoundaryError,
	"warning":             conversationBoundaryWarning,
}

func projectEventMessage(item conversationItem, raw json.RawMessage) (conversationItem, bool, bool, error) {
	var payload conversationEventPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return conversationItem{}, false, false, nil
	}
	switch payload.Type {
	case "user_message":
		text, truncated := clampItemText(payload.Message)
		item.Kind = conversationItemUserMessage
		item.Text = text
		item.Omitted = truncated || len(payload.Images) > 0
		item.family = conversationFamilyEventMessage
		return item, true, false, nil
	case "agent_message":
		text, truncated := clampItemText(payload.Message)
		item.Kind = conversationItemAssistantMessage
		item.Text = text
		item.Omitted = truncated
		item.family = conversationFamilyEventMessage
		return item, true, false, nil
	}
	marker, ok := eventBoundaryMarkers[payload.Type]
	if !ok {
		return conversationItem{}, false, false, nil
	}
	item.Kind = conversationItemBoundary
	item.Marker = marker
	text := payload.Reason
	switch payload.Type {
	case "thread_name_updated":
		text = payload.Name
		if text == "" {
			text = payload.Title
		}
	case "thread_goal_updated":
		text = payload.Status
		if payload.Goal != nil && payload.Goal.Status != "" {
			text = payload.Goal.Status
		}
	case "error", "warning":
		text = payload.Message
	}
	clamped, truncated := clampItemText(text)
	item.Text = clamped
	item.Omitted = truncated
	return item, true, false, nil
}

// rawToText unwraps a JSON field that may be a plain string or a structured
// value into display text without promising more than was persisted.
func rawToText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text
	}
	compact := &bytes.Buffer{}
	if json.Compact(compact, trimmed) == nil {
		return compact.String()
	}
	return string(trimmed)
}

// toolOutputText extracts display text from a persisted tool result. Outputs
// may be a plain string or an object carrying content items.
func toolOutputText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text
	}
	var structured struct {
		Output  json.RawMessage `json:"output"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_items"`
	}
	if json.Unmarshal(trimmed, &structured) != nil {
		return ""
	}
	var parts []string
	for _, content := range append(structured.Content, structured.ContentItems...) {
		if strings.TrimSpace(content.Text) != "" {
			parts = append(parts, content.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return rawToText(structured.Output)
}

// dedupeConversationMessages collapses the double persistence of one logical
// chat message. Codex rollouts write each user/assistant message twice — an
// event_msg user_message/agent_message record and a response_item message
// record carrying the same content — so emitting both families renders every
// message twice. The heuristic is deliberately narrow and stays a heuristic:
// only message items with identical clamped text, from different families,
// within conversationMessageDedupSpan emitted items of each other collapse.
// The first record's position (and ID) is kept for stable chronology; when
// the later record is the canonical response_item its projected fields win.
// Two genuinely repeated identical messages — same text sent twice — survive:
// they arrive from the same family or land outside the nearby window.
// Residual: a duplicate pair split across a page boundary or an incremental
// forward poll can still emit twice; collapsing across pages would require
// carrying dedup state in the cursor and is not worth it for a display path.
func dedupeConversationMessages(items []conversationItem) []conversationItem {
	if len(items) < 2 {
		return items
	}
	type sighting struct {
		ordinal int
		out     int
		family  string
		// paired marks a kept message that already absorbed its cross-family
		// twin; it cannot pair again, so a genuinely repeated identical
		// message is never eaten by its predecessor's duplicate.
		paired bool
	}
	recent := map[string][]sighting{}
	kept := make([]conversationItem, 0, len(items))
	for _, item := range items {
		ordinal := len(kept)
		if item.Kind != conversationItemUserMessage && item.Kind != conversationItemAssistantMessage {
			kept = append(kept, item)
			continue
		}
		key := item.Kind + "\x00" + item.Text
		duplicate := -1
		fresh := recent[key][:0]
		for _, s := range recent[key] {
			if ordinal-s.ordinal > conversationMessageDedupSpan {
				continue
			}
			fresh = append(fresh, s)
			if duplicate < 0 && !s.paired && s.family != item.family {
				duplicate = s.out
				fresh[len(fresh)-1].paired = true
			}
		}
		recent[key] = fresh
		if duplicate >= 0 {
			// Same logical message already shown: keep the earlier position,
			// but let the canonical response_item projection upgrade the kept
			// item's content fields (it carries the richer per-part flags).
			if item.family == conversationFamilyResponseItem && kept[duplicate].family != conversationFamilyResponseItem {
				id := kept[duplicate].ID
				kept[duplicate] = item
				kept[duplicate].ID = id
			}
			continue
		}
		recent[key] = append(recent[key], sighting{ordinal: ordinal, out: len(kept), family: item.family})
		kept = append(kept, item)
	}
	return kept
}

// clampItemText bounds display text at a rune boundary. The second result
// reports truncation so items can flag omitted content.
func clampItemText(text string) (string, bool) {
	if utf8.RuneCountInString(text) <= maxItemTextRunes {
		return text, false
	}
	runes := []rune(text)
	return string(runes[:maxItemTextRunes]), true
}

// sanitizeMarker keeps record-type markers printable and bounded. Markers
// come from untrusted file content and feed UI labels.
func sanitizeMarker(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if builder.Len() >= maxMarkerRunes {
			break
		}
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteByte('_')
		}
	}
	out := builder.String()
	if out == "" {
		return "record"
	}
	return out
}
