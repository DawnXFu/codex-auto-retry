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
// from the same family. The same bound caps the fingerprint ring carried in
// cursors: a seam-split pair can sit at most this many emitted items apart.
const conversationMessageDedupSpan = 8

// readerCursorVersion identifies the opaque cursor schema. Version 2 added
// the dedup fingerprint ring (the d field); version 3 generalizes positions
// to a constituent file index plus per-file fingerprints so one conversation
// can span multiple rollout files. Earlier cursors cannot describe the file
// set and are rejected as invalid so clients re-anchor at the latest page —
// permitted under the opaque-cursor contract.
const readerCursorVersion = 3

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
	// fileIndex records which constituent rollout file produced the item in
	// a multi-segment read; it feeds cross-file dedup and is internal-only.
	fileIndex int
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

// readerCursorFile fingerprints one constituent rollout file: its first-line
// hash plus size and mtime at issue. A cursor carries one entry per resolved
// file so truncation, replacement, or rotation of ANY segment invalidates it;
// the last segment is allowed to grow (append-only tail writes keep forward
// cursors alive).
type readerCursorFile struct {
	Head string `json:"h"`
	Size int64  `json:"s"`
	Mod  int64  `json:"m"`
}

// readerCursor is the opaque position token for item pages. F+O address one
// byte position inside the resolved file set (file index + offset within that
// file) and Files fingerprints the whole constituent set. D is a bounded
// ring — at most conversationMessageDedupSpan entries — of short fingerprints
// for the emitted items nearest the cursor's seam, so a dual-written pair or
// a cross-file duplicated record split across a page boundary or a forward
// poll still collapses. Fingerprints are hashes only; raw message text never
// enters a cursor.
type readerCursor struct {
	V     int                `json:"v"`
	F     int                `json:"f"`
	O     int64              `json:"o"`
	Files []readerCursorFile `json:"w"`
	D     []string           `json:"d,omitempty"`
}

func encodeReaderCursor(fileIndex int, off int64, files []readerCursorFile, dedup []string) string {
	data, err := json.Marshal(readerCursor{V: readerCursorVersion, F: fileIndex, O: off, Files: files, D: dedup})
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
	if cursor.V != readerCursorVersion || cursor.F < 0 || cursor.O < 0 ||
		len(cursor.Files) == 0 || len(cursor.Files) > maxCatalogResolveCandidates ||
		cursor.F >= len(cursor.Files) ||
		len(cursor.D) > conversationMessageDedupSpan {
		return nil, fmt.Errorf("%w", errConversationCursorInvalid)
	}
	for _, witness := range cursor.Files {
		head, err := hex.DecodeString(witness.Head)
		if err != nil || len(head) != sha256.Size || witness.Size < 0 || witness.Mod < 0 {
			return nil, fmt.Errorf("%w", errConversationCursorInvalid)
		}
	}
	for _, fingerprint := range cursor.D {
		decoded, err := hex.DecodeString(fingerprint)
		if err != nil || len(decoded) != conversationMessageFingerprintBytes {
			return nil, fmt.Errorf("%w", errConversationCursorInvalid)
		}
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

// readItems resolves the opaque identity to the whitelisted rollout files of
// a conversation, then returns one bounded page of projected items. Each file
// is re-validated after open so a swapped file can never borrow the resolved
// identity (TOCTOU).
func (r *conversationReader) readItems(ctx context.Context, rootID, threadID, cursor, direction string, limit int) (conversationItemPage, error) {
	direction, err := parseConversationDirection(direction)
	if err != nil {
		return conversationItemPage{}, err
	}
	resolved, err := r.catalog.resolveAll(rootID, threadID)
	if err != nil {
		return conversationItemPage{}, err
	}
	return r.readResolvedItems(ctx, resolved, cursor, direction, limit)
}

// conversationOpenFile is one constituent rollout file held open for a read,
// with the witness data cursors carry: first-line hash, size, mtime, and the
// end of persisted content (before any trailing partial line).
type conversationOpenFile struct {
	file    *os.File
	size    int64
	mod     int64
	tail    int64
	headHex string
}

// errConversationIdentityMismatch marks a file that opened cleanly but no
// longer carries a session_meta for the thread — a post-resolve swap, never
// a usable segment.
var errConversationIdentityMismatch = errors.New("conversation identity mismatch")

// openConversationFile opens one resolved segment and re-validates its
// persisted identity on the open handle — never a second path open — so a
// file swapped between resolve and read cannot borrow the identity.
func openConversationFile(path, threadID string) (*conversationOpenFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: rollout open", errConversationNotFound)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%w: rollout stat", errConversationNotFound)
	}
	if _, ok := inspectConversationManifestHandle(file, info, path, threadID); !ok {
		file.Close()
		return nil, errConversationIdentityMismatch
	}
	head, ok := readFirstRolloutLine(file, info.Size())
	if !ok {
		file.Close()
		return nil, errConversationIdentityMismatch
	}
	headSum := sha256.Sum256(head)
	return &conversationOpenFile{
		file:    file,
		size:    info.Size(),
		mod:     info.ModTime().UnixNano(),
		tail:    rolloutTailOffset(file, info.Size()),
		headHex: hex.EncodeToString(headSum[:]),
	}, nil
}

// conversationScanBudget is the single byte/time budget shared by every
// constituent file of one read: a multi-segment page gets the same scan
// bounds as a single-file one, not a per-file multiplication.
type conversationScanBudget struct {
	deadline time.Time
	scanned  int64
}

// readResolvedItems reads one page from an already-whitelisted conversation,
// treating the file set as one virtual concatenated log ordered oldest file
// first. Splitting it from resolveAll keeps the identity mapping the only
// place that accepts (root_id, thread_id).
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

	// Open every constituent file and witness it. With a cursor, the whole
	// minted set must still exist — a vanished or re-identified segment means
	// the file set changed under the client.
	files := make([]*conversationOpenFile, 0, len(resolved.Files))
	identityMissed := false
	for _, segment := range resolved.Files {
		file, err := openConversationFile(segment.Path, threadID)
		if err != nil {
			if errors.Is(err, errConversationIdentityMismatch) {
				identityMissed = true
			}
			continue
		}
		files = append(files, file)
	}
	defer func() {
		for _, file := range files {
			file.file.Close()
		}
	}()
	if position != nil && len(files) != len(position.Files) {
		return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
	}
	if len(files) == 0 {
		if identityMissed {
			return conversationItemPage{
				Items:    []conversationItem{},
				State:    conversationStateUnsupported,
				Archived: resolved.Archived,
				Kind:     resolved.Kind,
			}, nil
		}
		return conversationItemPage{}, fmt.Errorf("%w: rollout open", errConversationNotFound)
	}
	if position != nil {
		// Every constituent file is checked against its minted fingerprint.
		// The newest segment may grow (appended records); earlier segments
		// are frozen history — any size or mtime drift invalidates.
		last := len(files) - 1
		for i, file := range files {
			witness := position.Files[i]
			if file.headHex != witness.Head || file.size < witness.Size || file.mod < witness.Mod {
				return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
			}
			if i != last && (file.size != witness.Size || file.mod != witness.Mod) {
				return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
			}
		}
		target := files[position.F]
		if position.O > target.size {
			return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
		}
		if position.O > 0 {
			// Issued offsets only ever sit on record boundaries: byte O-1
			// must be the newline terminating the previous record.
			boundary := make([]byte, 1)
			if _, err := target.file.ReadAt(boundary, position.O-1); err != nil || boundary[0] != '\n' {
				return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
			}
		}
	}

	page := conversationItemPage{
		Items:    []conversationItem{},
		State:    conversationStateCompletePersisted,
		Archived: resolved.Archived,
		Kind:     resolved.Kind,
	}
	budget := &conversationScanBudget{deadline: time.Now().Add(maxReaderScanDuration)}
	// The incoming cursor's fingerprint ring covers the seam between this
	// page and what the client already rendered; leftovers the scan does not
	// consume carry into freshly minted cursors.
	var dedupRing []string
	if position != nil {
		dedupRing = position.D
	}
	lastFile := len(files) - 1
	witnesses := conversationWitnesses(files)
	tailFile := lastFile
	tailOff := files[lastFile].tail

	if direction == conversationDirectionForward {
		startFile, start := 0, int64(0)
		if position != nil {
			startFile, start = position.F, position.O
		}
		items, resumeFile, resumeOff, hasMore, partial, err := scanConversationFilesForward(ctx, files, startFile, start, limit, budget)
		if err != nil {
			return conversationItemPage{}, err
		}
		items, dedupRing = suppressCrossPageDuplicates(items, dedupRing, direction)
		page.Items = dedupeConversationMessages(suppressEarlierFileDuplicates(items))
		if partial {
			page.State = conversationStatePartial
		}
		// A forward anchor seams at the page's newest edge: keep the K
		// latest fingerprints — unconsumed leftovers are the older emitted
		// messages, this page's are newer. Record fingerprints are minted
		// only for items from a non-tail file: only they can twin a
		// later-segment duplicate.
		pageFps := conversationPageFingerprints(page.Items, func(item conversationItem) bool {
			return item.fileIndex < lastFile
		})
		tailRing := mintForwardRing(dedupRing, pageFps)
		page.TailCursor = encodeReaderCursor(tailFile, tailOff, witnesses, tailRing)
		if hasMore {
			page.NextCursor = encodeReaderCursor(resumeFile, resumeOff, witnesses, tailRing)
		}
		return page, nil
	}

	endFile, end := lastFile, tailOff
	if position != nil {
		if position.O > files[position.F].tail {
			return conversationItemPage{}, fmt.Errorf("%w", errConversationFileChanged)
		}
		endFile, end = position.F, position.O
	}
	items, resumeFile, resumeOff, hasMore, partial, err := scanConversationFilesBackward(ctx, files, endFile, end, limit, budget)
	if err != nil {
		return conversationItemPage{}, err
	}
	items, dedupRing = suppressCrossPageDuplicates(items, dedupRing, direction)
	page.Items = dedupeConversationMessages(suppressEarlierFileDuplicates(items))
	if partial {
		page.State = conversationStatePartial
	}
	// The tail anchor faces appended content; the chronologically newest
	// emitted fingerprints are the unconsumed leftovers (they describe the
	// newer page this page was paged from), then this page's own tail.
	pageFpsTail := conversationPageFingerprints(page.Items, func(item conversationItem) bool {
		return item.fileIndex < lastFile
	})
	tailRing := mintBackwardTailRing(pageFpsTail, dedupRing)
	page.TailCursor = encodeReaderCursor(tailFile, tailOff, witnesses, tailRing)
	if hasMore {
		// The older seam sits at this page's oldest edge: keep the K
		// fingerprints of the first emitted messages, padding with
		// unconsumed leftovers when the page is thin. Record fingerprints
		// are minted only for items from a later segment: only they can
		// twin an earlier-file duplicate on the next page.
		pageFpsOlder := conversationPageFingerprints(page.Items, func(item conversationItem) bool {
			return item.fileIndex > 0
		})
		page.NextCursor = encodeReaderCursor(resumeFile, resumeOff, witnesses, mintBackwardRing(pageFpsOlder, dedupRing))
	}
	return page, nil
}

// conversationWitnesses snapshots the per-file fingerprints minted into
// cursors: head hash, size, and mtime of every constituent file.
func conversationWitnesses(files []*conversationOpenFile) []readerCursorFile {
	witnesses := make([]readerCursorFile, len(files))
	for i, file := range files {
		witnesses[i] = readerCursorFile{Head: file.headHex, Size: file.size, Mod: file.mod}
	}
	return witnesses
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

// scanConversationFilesBackward pages toward the head of the virtual log:
// file fileIndex from end (exclusive), then earlier files from each one's
// persisted tail. Items come out in chronological order. resume is the
// (file, offset) start of the oldest line this page processed — a continuing
// backward page ends there — and hasMore reports whether unprocessed content
// remains before it.
func scanConversationFilesBackward(ctx context.Context, files []*conversationOpenFile, fileIndex int, end int64, limit int, budget *conversationScanBudget) (items []conversationItem, resumeFile int, resumeOff int64, hasMore, partial bool, err error) {
	// Files are scanned newest-first but their pages assemble oldest-first:
	// each per-file page is already chronological internally.
	segments := make([][]conversationItem, 0, fileIndex+1)
	emitted := 0
	for fi := fileIndex; fi >= 0; fi-- {
		fileEnd := end
		if fi != fileIndex {
			fileEnd = files[fi].tail
		}
		if fileEnd <= 0 {
			continue // no persisted records in this segment
		}
		if emitted >= limit {
			// The limit landed exactly on a file boundary; the next page
			// resumes at this file's tail.
			return assembleSegments(segments), fi, fileEnd, true, partial, nil
		}
		page, boundary, more, degraded, scanErr := scanConversationBackward(ctx, files[fi], fi, fileEnd, limit-emitted, budget)
		if scanErr != nil {
			return nil, 0, 0, false, partial, scanErr
		}
		segments = append(segments, page)
		emitted += len(page)
		partial = partial || degraded
		if more {
			return assembleSegments(segments), fi, boundary, true, partial, nil
		}
	}
	return assembleSegments(segments), 0, 0, false, partial, nil
}

// assembleSegments flattens per-file pages scanned newest-first into the
// page's chronological order (oldest file first).
func assembleSegments(segments [][]conversationItem) []conversationItem {
	total := 0
	for _, segment := range segments {
		total += len(segment)
	}
	items := make([]conversationItem, 0, total)
	for i := len(segments) - 1; i >= 0; i-- {
		items = append(items, segments[i]...)
	}
	return items
}

// scanConversationBackward collects up to limit projected items scanning one
// file toward the head from end (exclusive). Items come out in chronological
// order. resume is the start offset of the oldest line this page processed:
// a continuing backward page ends there, so emitted content is never replayed
// and every issued offset sits after a newline (the cursor boundary
// invariant). hasMore reports whether unprocessed content remains before it.
// The shared budget carries byte and time bounds across constituent files.
func scanConversationBackward(ctx context.Context, file *conversationOpenFile, fileIndex int, end int64, limit int, budget *conversationScanBudget) (items []conversationItem, resume int64, hasMore, partial bool, err error) {
	collected := make([]conversationItem, 0, limit)
	lineEnd := end
	boundary := end
	pos := end
	stopped := false
	for pos > 0 && !stopped {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, partial, err
		}
		remaining := maxReaderScanBytes - budget.scanned
		if time.Now().After(budget.deadline) || remaining <= 0 {
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
		read, readErr := file.file.ReadAt(buffer, start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, 0, false, partial, readErr
		}
		buffer = buffer[:read]
		budget.scanned += int64(len(buffer))
		for i := len(buffer) - 1; i >= 0; i-- {
			if buffer[i] != '\n' {
				continue
			}
			lineStart := start + int64(i) + 1
			if lineStart < lineEnd {
				item, emit, degraded, itemErr := readConversationLine(file.file, fileIndex, lineStart, lineEnd)
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
		item, emit, degraded, itemErr := readConversationLine(file.file, fileIndex, 0, lineEnd)
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

// scanConversationFilesForward pages toward the tail of the virtual log:
// file fileIndex from start, then each later file from its head up to its
// persisted tail. resume is the (file, offset) of the first unread byte after
// the last processed line; hasMore reports whether more complete content
// sits between resume and the virtual log's end.
func scanConversationFilesForward(ctx context.Context, files []*conversationOpenFile, fileIndex int, start int64, limit int, budget *conversationScanBudget) (items []conversationItem, resumeFile int, resumeOff int64, hasMore, partial bool, err error) {
	collected := make([]conversationItem, 0, limit)
	for fi := fileIndex; fi < len(files); fi++ {
		fileStart := start
		if fi != fileIndex {
			fileStart = 0
		}
		if fileStart >= files[fi].tail {
			continue // nothing persisted in this segment
		}
		if len(collected) >= limit {
			// The limit landed exactly on a file boundary; the next page
			// resumes at this file's head.
			return collected, fi, fileStart, true, partial, nil
		}
		page, pos, more, degraded, scanErr := scanConversationForward(ctx, files[fi], fi, fileStart, files[fi].tail, limit-len(collected), budget)
		if scanErr != nil {
			return nil, 0, 0, false, partial, scanErr
		}
		collected = append(collected, page...)
		partial = partial || degraded
		if more {
			return collected, fi, pos, true, partial, nil
		}
	}
	return collected, 0, 0, false, partial, nil
}

// scanConversationForward projects complete lines of one file starting at
// start up to tail (the end of persisted content). resume is the byte offset
// of the first unread byte after the last processed line; hasMore reports
// whether more complete content sits between resume and tail. The shared
// budget carries byte and time bounds across constituent files.
func scanConversationForward(ctx context.Context, file *conversationOpenFile, fileIndex int, start, tail int64, limit int, budget *conversationScanBudget) (items []conversationItem, resume int64, hasMore, partial bool, err error) {
	if _, err := file.file.Seek(start, io.SeekStart); err != nil {
		return nil, 0, false, false, err
	}
	reader := bufio.NewReaderSize(file.file, int(maxReaderBlockBytes))
	pos := start
	lineStart := start
	collected := make([]conversationItem, 0, limit)
	var line []byte
	oversized := false
	for pos < tail {
		if err := ctx.Err(); err != nil {
			return nil, 0, false, partial, err
		}
		if time.Now().After(budget.deadline) || budget.scanned >= maxReaderScanBytes {
			return collected, pos, pos < tail, true, nil
		}
		fragment, readErr := reader.ReadSlice('\n')
		pos += int64(len(fragment))
		budget.scanned += int64(len(fragment))
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
				ID:        conversationItemID(fileIndex, lineStart),
				Kind:      conversationItemUnknown,
				Marker:    "oversized",
				Omitted:   true,
				fileIndex: fileIndex,
			})
		} else if complete {
			item, emit, degraded, itemErr := projectConversationLine(bytes.TrimSuffix(line, []byte{'\n'}), fileIndex, lineStart)
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
func readConversationLine(file *os.File, fileIndex int, start, end int64) (conversationItem, bool, bool, error) {
	length := end - start
	if length <= 0 {
		return conversationItem{}, false, false, nil
	}
	if length > maxReaderLineBytes {
		return conversationItem{
			ID:        conversationItemID(fileIndex, start),
			Kind:      conversationItemUnknown,
			Marker:    "oversized",
			Omitted:   true,
			fileIndex: fileIndex,
		}, true, true, nil
	}
	buffer := make([]byte, length)
	if _, err := file.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
		return conversationItem{}, false, false, err
	}
	return projectConversationLine(buffer, fileIndex, start)
}

// conversationItemID is the display id of a projected record: file index
// plus byte offset inside it, so two segments can never mint the same id.
func conversationItemID(fileIndex int, offset int64) string {
	return fmt.Sprintf("i%x-%x", fileIndex, offset)
}

// projectConversationLine maps one persisted record to a display item. The
// emit result is false for recognized record families the chat view
// deliberately does not show (settings, quota, reasoning, streaming noise).
// degraded is true when the record could not be faithfully projected.
func projectConversationLine(line []byte, fileIndex int, offset int64) (conversationItem, bool, bool, error) {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return conversationItem{}, false, false, nil
	}
	item := conversationItem{ID: conversationItemID(fileIndex, offset), fileIndex: fileIndex}
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

// conversationMessageFingerprintBytes bounds the message fingerprint carried
// in cursor dedup rings: the first 8 bytes of a SHA-256 over the message's
// dedup identity. Sixteen hex chars per entry keeps the opaque cursor small
// while a collision costs at most one wrongly collapsed display duplicate.
const conversationMessageFingerprintBytes = 8

// conversationMessageFingerprint hashes a message's dedup identity — kind,
// record family, and normalized text — into the short cursor-safe
// fingerprint carried in the ring. Raw text never enters the cursor. The
// family is part of the hash so a fingerprint only ever matches the opposite
// record family of one logical message: ring entries are minted with the
// emitting item's own family and looked up with the sibling family.
func conversationMessageFingerprint(kind, family, text string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + family + "\x00" + text))
	return hex.EncodeToString(sum[:conversationMessageFingerprintBytes])
}

// conversationRecordFingerprint hashes a record's cross-file identity —
// kind, persisted timestamp, and normalized text — into the same short
// cursor-safe fingerprint space as conversationMessageFingerprint. Two
// files carrying the same record (a copied resume prefix, a mirrored
// segment) produce identical fingerprints; the "record" domain tag keeps
// them disjoint from dual-family message fingerprints.
func conversationRecordFingerprint(item conversationItem) string {
	sum := sha256.Sum256([]byte("record\x00" + item.Kind + "\x00" + item.Timestamp + "\x00" + item.Text))
	return hex.EncodeToString(sum[:conversationMessageFingerprintBytes])
}

// conversationSiblingFamily returns the other persisted record family that
// dual-writes a chat message. Non-message items carry no family and have no
// sibling.
func conversationSiblingFamily(family string) string {
	switch family {
	case conversationFamilyEventMessage:
		return conversationFamilyResponseItem
	case conversationFamilyResponseItem:
		return conversationFamilyEventMessage
	default:
		return ""
	}
}

// suppressCrossPageDuplicates collapses dual-family twins the in-page pass
// cannot see: a message whose sibling record was emitted on a previous page
// (backward seam) or before a forward poll's cursor position. Each ring
// fingerprint suppresses at most one item — consume-on-use, the same
// paired-flag semantics as dedupeConversationMessages — so a genuinely
// repeated identical message survives once its predecessor's fingerprint was
// claimed by that sibling. Items are visited from the seam inward — backward
// pages seam at their newest item, forward pages at their oldest — so the
// item closest to the seam claims a contested fingerprint first. The
// surviving items in chronological order and the unconsumed ring entries are
// returned; the leftovers roll into freshly minted cursors.
func suppressCrossPageDuplicates(items []conversationItem, ring []string, direction string) ([]conversationItem, []string) {
	if len(ring) == 0 || len(items) == 0 {
		return items, ring
	}
	used := make([]bool, len(ring))
	suppressed := make([]bool, len(items))
	for step := 0; step < len(items); step++ {
		i := step
		if direction == conversationDirectionBackward {
			i = len(items) - 1 - step
		}
		// Two fingerprint kinds share the ring: the sibling-family hash of a
		// dual-written message, and the record fingerprint of an item already
		// emitted from a neighboring segment file.
		var messageFp string
		if sibling := conversationSiblingFamily(items[i].family); sibling != "" {
			messageFp = conversationMessageFingerprint(items[i].Kind, sibling, items[i].Text)
		}
		recordFp := conversationRecordFingerprint(items[i])
		for j := range ring {
			if !used[j] && (ring[j] == messageFp || ring[j] == recordFp) {
				used[j] = true
				suppressed[i] = true
				break
			}
		}
	}
	kept := make([]conversationItem, 0, len(items))
	for i := range items {
		if !suppressed[i] {
			kept = append(kept, items[i])
		}
	}
	leftover := make([]string, 0, len(ring))
	for j := range ring {
		if !used[j] {
			leftover = append(leftover, ring[j])
		}
	}
	return kept, leftover
}

// conversationPageFingerprints hashes the page's emitted items in
// chronological order. Two fingerprint kinds interleave: chat messages carry
// their family fingerprint for dual-write collapse, and items matching
// crossFile (a file-index predicate) additionally carry a record fingerprint
// so an identical record emitted from a neighboring segment file still
// collapses across a page seam. Only items that actually render carry
// fingerprints — a record suppressed before emission never enters the ring.
func conversationPageFingerprints(items []conversationItem, crossFile func(item conversationItem) bool) []string {
	fingerprints := make([]string, 0, len(items))
	for _, item := range items {
		if item.family != "" {
			fingerprints = append(fingerprints, conversationMessageFingerprint(item.Kind, item.family, item.Text))
		}
		if crossFile != nil && crossFile(item) {
			fingerprints = append(fingerprints, conversationRecordFingerprint(item))
		}
	}
	return fingerprints
}

// conversationFileDedupBound caps the cross-file duplicate window inside one
// page: fingerprints of already-emitted items live in a bounded FIFO map, so
// a copied prefix longer than this still leaves the oldest duplicates
// rendered. The bound trades a worst-case double-render for a fixed memory
// cost independent of segment size.
const conversationFileDedupBound = 256

// suppressEarlierFileDuplicates drops items from a segment file when an
// identical record (same kind, timestamp, and normalized text) was already
// emitted from an earlier file — resumed rollouts can re-persist copied
// records at their head, and the earlier segment's copy keeps the position.
// Items arrive in virtual-log order so file indices are nondecreasing; the
// earlier occurrence's file index stays recorded so a within-file duplicate
// is never suppressed by its own twin.
func suppressEarlierFileDuplicates(items []conversationItem) []conversationItem {
	if len(items) < 2 {
		return items
	}
	multiFile := false
	for i := range items {
		if items[i].fileIndex > 0 {
			multiFile = true
			break
		}
	}
	if !multiFile {
		return items
	}
	type sighting struct{ fileIndex int }
	seen := make(map[string]sighting, conversationFileDedupBound)
	order := make([]string, 0, conversationFileDedupBound)
	kept := make([]conversationItem, 0, len(items))
	for _, item := range items {
		fp := conversationRecordFingerprint(item)
		if first, ok := seen[fp]; ok {
			if first.fileIndex < item.fileIndex {
				continue // identical record already emitted from an earlier file
			}
			kept = append(kept, item)
			continue
		}
		if len(order) >= conversationFileDedupBound {
			delete(seen, order[0])
			order = order[1:]
		}
		seen[fp] = sighting{fileIndex: item.fileIndex}
		order = append(order, fp)
		kept = append(kept, item)
	}
	return kept
}

// mintForwardRing keeps the K newest fingerprints for a forward-facing
// anchor (tail_cursor, or next_cursor on a forward page): unconsumed
// leftover entries describe older emitted messages, this page's are newer.
func mintForwardRing(leftover, page []string) []string {
	ring := make([]string, 0, conversationMessageDedupSpan)
	ring = append(ring, leftover...)
	ring = append(ring, page...)
	if len(ring) > conversationMessageDedupSpan {
		ring = ring[len(ring)-conversationMessageDedupSpan:]
	}
	return ring
}

// mintBackwardTailRing keeps the K fingerprints nearest the file tail for a
// backward page's tail anchor. Unconsumed leftovers describe messages on the
// newer page this page was paged from — chronologically newer than anything
// on this page — so they sit at the newest end of the ring.
func mintBackwardTailRing(page, leftover []string) []string {
	ring := make([]string, 0, conversationMessageDedupSpan)
	ring = append(ring, page...)
	ring = append(ring, leftover...)
	if len(ring) > conversationMessageDedupSpan {
		ring = ring[len(ring)-conversationMessageDedupSpan:]
	}
	return ring
}

// mintBackwardRing keeps the K fingerprints nearest the older seam for a
// backward next_cursor: this page's first emitted messages, padded by
// unconsumed leftover entries when the page carries fewer than K.
func mintBackwardRing(page, leftover []string) []string {
	ring := make([]string, 0, conversationMessageDedupSpan)
	ring = append(ring, page...)
	ring = append(ring, leftover...)
	if len(ring) > conversationMessageDedupSpan {
		ring = ring[:conversationMessageDedupSpan]
	}
	return ring
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
// they arrive from the same family or land outside the nearby window. Pairs
// split across page boundaries or forward-poll appends are handled earlier by
// the cursor fingerprint ring (suppressCrossPageDuplicates); this pass is
// the in-page first line of that collapse.
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
