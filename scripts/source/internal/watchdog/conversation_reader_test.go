package watchdog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	readerThreadC1 = "019fc0c1-0001-7c01-8001-0000000000c1"
	readerThreadC2 = "019fc0c1-0002-7c02-8002-0000000000c2"
	readerThreadC3 = "019fc0c1-0003-7c03-8003-0000000000c3"
)

// readerFixtureRoot points at the synthetic homeC conversation fixtures under
// testdata/conversations. Reader tests must never touch a real Codex home.
func readerFixtureRoot(t *testing.T) sessionRoot {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("testdata", "conversations", "homeC"))
	if err != nil {
		t.Fatal(err)
	}
	return sessionRoot{
		CodexHome:        base,
		Sessions:         filepath.Join(base, "sessions"),
		ArchivedSessions: filepath.Join(base, "archived_sessions"),
	}
}

func readerFixtureReader(t *testing.T) (*conversationReader, sessionRoot) {
	t.Helper()
	root := readerFixtureRoot(t)
	return newConversationReader(newConversationCatalog([]sessionRoot{root})), root
}

func itemKinds(items []conversationItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		label := item.Kind
		if item.Marker != "" {
			label += ":" + item.Marker
		}
		out = append(out, label)
	}
	return out
}

func TestConversationReaderProjectsLatestPageBackward(t *testing.T) {
	reader, root := readerFixtureReader(t)
	page, err := reader.readItems(context.Background(), conversationRootID(root), readerThreadC1, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != "complete_persisted" {
		t.Fatalf("state = %q, want complete_persisted", page.State)
	}
	if page.Archived || page.Kind != "ordinary" {
		t.Fatalf("page flags wrong: %+v", page)
	}
	got := itemKinds(page.Items)
	want := []string{
		"boundary:session",
		"boundary:turn_started",
		"user_message",
		"user_message",
		"tool_call",
		"tool_result",
		"tool_call",
		"tool_result",
		"assistant_message",
		"assistant_message",
		"boundary:turn_complete",
		"boundary:turn_aborted",
		"boundary:compaction",
		"tool_call",
		"tool_call",
		"boundary:thread_renamed",
		"unknown:future_thing",
		"unknown:mystery_envelope",
	}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	// The newest persisted record ends the page; a next cursor would mean
	// older content is still unread, which is true here only via paging below.
	if page.TailCursor == "" {
		t.Fatal("tail cursor missing: forward polling would have no anchor")
	}
}

func TestConversationReaderItemContentIsBoundedAndSafe(t *testing.T) {
	reader, root := readerFixtureReader(t)
	page, err := reader.readItems(context.Background(), conversationRootID(root), readerThreadC1, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	byText := map[string]conversationItem{}
	var toolCalls, toolResults int
	for _, item := range page.Items {
		byText[item.Text] = item
		if item.Timestamp != "" && !strings.HasPrefix(item.Timestamp, "2026-10-03T10:") {
			t.Fatalf("unexpected timestamp %q", item.Timestamp)
		}
		switch item.Kind {
		case "tool_call":
			toolCalls++
			if !item.Collapsed {
				t.Fatalf("tool call not marked collapsible: %+v", item)
			}
		case "tool_result":
			toolResults++
			if !item.Collapsed {
				t.Fatalf("tool result not marked collapsible: %+v", item)
			}
		}
	}
	user := byText["here is more context"]
	if user.Kind != "user_message" || !user.Omitted {
		t.Fatalf("user message with attachment wrong: %+v", user)
	}
	assistant := byText["Fixed the race in the poller."]
	if assistant.Kind != "assistant_message" || assistant.Timestamp != "2026-10-03T10:02:10Z" {
		t.Fatalf("assistant message wrong: %+v", assistant)
	}
	if toolCalls != 4 || toolResults != 2 {
		t.Fatalf("tool items = %d calls / %d results, want 4/2", toolCalls, toolResults)
	}
	// The watchdog's own recovery notice is a developer-role record and must
	// never surface as conversation content.
	for _, item := range page.Items {
		if strings.Contains(item.Text, "codex-auto-retry:subagent-empty-response-recovery") {
			t.Fatalf("recovery notice leaked into items: %+v", item)
		}
	}
}

func TestConversationReaderPaginatesBackwardToHistoryStart(t *testing.T) {
	reader, root := readerFixtureReader(t)
	rootID := conversationRootID(root)

	full, err := reader.readItems(context.Background(), rootID, readerThreadC1, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	var pagesSeen [][]conversationItem
	cursor := ""
	for pages := 0; ; pages++ {
		page, err := reader.readItems(context.Background(), rootID, readerThreadC1, cursor, "backward", 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 5 {
			t.Fatalf("page exceeded limit: %d items", len(page.Items))
		}
		pagesSeen = append(pagesSeen, page.Items)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("backward pagination did not terminate")
		}
	}
	if len(pagesSeen) != 4 {
		t.Fatalf("expected 4 pages of 5 for 18 items, got %d", len(pagesSeen))
	}
	// Pages arrive latest-first; prepending older pages reconstructs the
	// chronological projection the single large page returned.
	var seen []conversationItem
	for i := len(pagesSeen) - 1; i >= 0; i-- {
		seen = append(seen, pagesSeen[i]...)
	}
	if len(seen) != len(full.Items) {
		t.Fatalf("paginated union has %d items, full page has %d", len(seen), len(full.Items))
	}
	for i := range full.Items {
		if seen[i].ID != full.Items[i].ID || seen[i].Kind != full.Items[i].Kind {
			t.Fatalf("page order diverged at %d: %+v vs %+v", i, seen[i], full.Items[i])
		}
	}
}

func TestConversationReaderCursorRejectsForeignThreadAndGarbage(t *testing.T) {
	reader, root := readerFixtureReader(t)
	rootID := conversationRootID(root)
	page, err := reader.readItems(context.Background(), rootID, readerThreadC1, "", "backward", 5)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	if _, err := reader.readItems(context.Background(), rootID, readerThreadC1, "not-a-cursor", "backward", 5); err == nil {
		t.Fatal("garbage cursor accepted")
	}
	// A cursor minted for one thread must not page a different thread.
	other, err := reader.readItems(context.Background(), rootID, readerThreadC3, "", "backward", 5)
	if err != nil {
		t.Fatal(err)
	}
	if other.NextCursor != "" {
		if _, err := reader.readItems(context.Background(), rootID, readerThreadC1, other.NextCursor, "backward", 5); err == nil {
			t.Fatal("foreign-thread cursor accepted")
		}
	}
	if _, err := reader.readItems(context.Background(), rootID, readerThreadC1, page.NextCursor, "sideways", 5); err == nil {
		t.Fatal("unknown direction accepted")
	}
}

// writeReaderRollout creates a synthetic rollout file inside a temp home and
// returns the root, thread id, and file path for incremental tests.
func writeReaderRollout(t *testing.T, threadID string, lines []string) (sessionRoot, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-03T10-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sessionRoot{CodexHome: home, Sessions: filepath.Join(home, "sessions")}, path
}

func appendRollout(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func readerMetaLine(threadID, ts string) string {
	return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli"}}`
}

func TestConversationReaderPartialLineWaitsForCompletion(t *testing.T) {
	threadID := "019fc0c1-00aa-7c0a-800a-0000000000aa"
	root, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"first"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	// Simulate an append in progress: a record without its newline is not
	// persisted yet and must not project.
	appendRollout(t, path, `{"timestamp":"2026-10-03T10:02:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"second`)

	page, err := reader.readItems(context.Background(), rootID, threadID, "", "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("partial line projected early: %+v", page.Items)
	}
	if page.NextCursor != "" || page.TailCursor == "" {
		t.Fatalf("cursor state wrong: next=%q tail=%q", page.NextCursor, page.TailCursor)
	}

	// Finish the line; a forward poll from the tail anchor picks it up alone.
	appendRollout(t, path, ` part"}}`+"\n")
	page, err = reader.readItems(context.Background(), rootID, threadID, page.TailCursor, "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != "user_message" || page.Items[0].Text != "second part" {
		t.Fatalf("completed append not projected incrementally: %+v", page.Items)
	}
	if page.State != "complete_persisted" {
		t.Fatalf("state = %q", page.State)
	}
}

func TestConversationReaderBackwardThenForwardFollowsAppends(t *testing.T) {
	threadID := "019fc0c1-00bb-7c0b-800b-0000000000bb"
	root, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, threadID, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.TailCursor == "" {
		t.Fatalf("initial page wrong: %+v", page)
	}
	appendRollout(t, path, `{"timestamp":"2026-10-03T10:02:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer two"}]}}`+"\n")

	follow, err := reader.readItems(context.Background(), rootID, threadID, page.TailCursor, "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(follow.Items) != 1 || follow.Items[0].Text != "answer two" {
		t.Fatalf("forward poll from tail cursor = %+v", follow.Items)
	}
	// A repeated poll with no new appends returns an empty page, not a replay.
	again, err := reader.readItems(context.Background(), rootID, threadID, follow.TailCursor, "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 0 {
		t.Fatalf("re-poll replayed items: %+v", again.Items)
	}
}

func TestConversationReaderCursorInvalidatesOnTruncateReplaceAndRewind(t *testing.T) {
	threadID := "019fc0c1-00cc-7c0c-800c-0000000000cc"
	root, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"one"}}`,
		`{"timestamp":"2026-10-03T10:02:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"two"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, threadID, "", "backward", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" || page.TailCursor == "" {
		t.Fatal("expected cursors to invalidate")
	}

	// Truncation shrinks the file below the issued position.
	if err := os.WriteFile(path, []byte(readerMetaLine(threadID, "2026-10-03T10:00:00.000Z")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readItems(context.Background(), rootID, threadID, page.NextCursor, "backward", 2); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("truncated file did not invalidate cursor: %v", err)
	}

	// Replacement keeps the thread id but rewrites the head record.
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		`{"timestamp":"2026-10-03T12:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli","title":"rotated"}}`,
		`{"timestamp":"2026-10-03T12:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"new file"}}`,
		`{"timestamp":"2026-10-03T12:02:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"more lines here"}}`,
		`{"timestamp":"2026-10-03T12:03:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"extra tail record"}}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readItems(context.Background(), rootID, threadID, page.TailCursor, "forward", 2); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("replaced file did not invalidate cursor: %v", err)
	}
	if _, err := reader.readItems(context.Background(), rootID, threadID, page.NextCursor, "backward", 2); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("replaced file did not invalidate backward cursor: %v", err)
	}

	// An mtime rewind on otherwise identical content is treated as rotation.
	page, err = reader.readItems(context.Background(), rootID, threadID, "", "backward", 2)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readItems(context.Background(), rootID, threadID, page.NextCursor, "backward", 2); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("mtime regression did not invalidate cursor: %v", err)
	}
}

func TestConversationReaderOversizedAndUnreadableLinesBecomePlaceholders(t *testing.T) {
	threadID := "019fc0c1-00dd-7c0d-800d-0000000000dd"
	defer func(cap int64) { maxReaderLineBytes = cap }(maxReaderLineBytes)
	maxReaderLineBytes = 256
	big := strings.Repeat("x", 600)
	root, _ := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"` + big + `"}}`,
		`{corrupt line`,
		`{"timestamp":"2026-10-03T10:03:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))

	page, err := reader.readItems(context.Background(), conversationRootID(root), threadID, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != conversationStatePartial {
		t.Fatalf("oversized/unreadable records should mark page partial: %q", page.State)
	}
	got := itemKinds(page.Items)
	want := []string{
		"boundary:session",
		"unknown:oversized",
		"unknown:unreadable",
		"assistant_message",
	}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestConversationReaderScanBudgetMarksPartial(t *testing.T) {
	reader, root := readerFixtureReader(t)
	defer func(budget int64) { maxReaderScanBytes = budget }(maxReaderScanBytes)
	maxReaderScanBytes = 200

	page, err := reader.readItems(context.Background(), conversationRootID(root), readerThreadC1, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != conversationStatePartial {
		t.Fatalf("scan budget exhaustion should mark page partial: %q", page.State)
	}
	if page.NextCursor == "" {
		t.Fatal("bounded scan must still hand back a continuation cursor")
	}

	// The time bound reports partial the same way; a deadline in the past
	// stops the scan deterministically before the first block.
	defer func(d time.Duration) { maxReaderScanDuration = d }(maxReaderScanDuration)
	maxReaderScanDuration = -time.Second
	page, err = reader.readItems(context.Background(), conversationRootID(root), readerThreadC1, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != conversationStatePartial {
		t.Fatalf("expired deadline should mark page partial: %q", page.State)
	}
}

func TestConversationReaderHonorsContextCancel(t *testing.T) {
	reader, root := readerFixtureReader(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reader.readItems(ctx, conversationRootID(root), readerThreadC1, "", "backward", 0); err == nil {
		t.Fatal("canceled context did not abort the read")
	}
}

// Real Codex rollouts persist one logical chat message twice — an event_msg
// user_message/agent_message record and a response_item message record with
// the same content — and the reader must not render it twice. The collapse is
// a conservative heuristic: identical text, different family, nearby.
func TestConversationReaderDeduplicatesDualWrittenMessages(t *testing.T) {
	threadID := "019fc0c1-00dd-7d0d-800d-0000000000dd"
	root, _ := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:00:10.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		// One logical user message, written by both record families. The
		// event_msg copy lacks the attachment flag the response_item copy
		// carries, so the canonical projection upgrades the kept item.
		`{"timestamp":"2026-10-03T10:00:20.000Z","type":"event_msg","payload":{"type":"user_message","message":"same question"}}`,
		`{"timestamp":"2026-10-03T10:00:20.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"same question"},{"type":"input_image","image_url":"file:///C:/shots/b.png"}]}}`,
		// One logical assistant message, response_item first this time.
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same answer"}]}}`,
		`{"timestamp":"2026-10-03T10:01:00.200Z","type":"event_msg","payload":{"type":"agent_message","message":"same answer"}}`,
		`{"timestamp":"2026-10-03T10:01:30.000Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"same answer"}}`,
		// A genuinely repeated identical message — same family, sent twice —
		// must not collapse.
		`{"timestamp":"2026-10-03T10:02:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"yes"}}`,
		`{"timestamp":"2026-10-03T10:02:30.000Z","type":"event_msg","payload":{"type":"user_message","message":"yes"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, threadID, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := itemKinds(page.Items)
	want := []string{
		"boundary:session",
		"boundary:turn_started",
		"user_message",
		"assistant_message",
		"boundary:turn_complete",
		"user_message",
		"user_message",
	}
	if len(got) != len(want) {
		t.Fatalf("dual-written messages not deduplicated: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	user := page.Items[2]
	if user.Text != "same question" || !user.Omitted {
		t.Fatalf("canonical response_item projection lost its omitted flag: %+v", user)
	}
	if user.ID == "" {
		t.Fatal("deduped item lost its id")
	}
}

// The in-page dedup window cannot reach across a page boundary: a
// dual-written pair split by pagination — the response_item copy on the
// newest page, its event_msg twin the last record of the older page — would
// render twice. The cursor's fingerprint ring carries the seam-side message
// fingerprints forward so the twin is still suppressed.
func TestConversationReaderDedupesPairAcrossBackwardPageBoundary(t *testing.T) {
	threadID := "019fc0c1-00ee-7e0e-800e-0000000000ee"
	root, _ := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:00:10.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"timestamp":"2026-10-03T10:00:20.000Z","type":"event_msg","payload":{"type":"user_message","message":"seam pair"}}`,
		`{"timestamp":"2026-10-03T10:00:20.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"seam pair"}]}}`,
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	// Page 1 (newest-first, limit=2) takes the turn_complete boundary and
	// the response_item copy; the event_msg twin stays just below the seam.
	page, err := reader.readItems(context.Background(), rootID, threadID, "", "backward", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a continuation cursor for the split pair")
	}
	if got := itemKinds(page.Items); len(got) != 2 || got[0] != "user_message" || got[1] != "boundary:turn_complete" {
		t.Fatalf("page 1 wrong: %v", got)
	}

	page2, err := reader.readItems(context.Background(), rootID, threadID, page.NextCursor, "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := itemKinds(page2.Items)
	want := []string{"boundary:session", "boundary:turn_started"}
	if len(got) != len(want) {
		t.Fatalf("cross-page twin not suppressed: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// Forward polling has the same seam: when the response_item copy of a
// message is appended only after the tail cursor was minted, the next poll
// must suppress it instead of replaying the message.
func TestConversationReaderDedupesForwardAppendAcrossPolls(t *testing.T) {
	threadID := "019fc0c1-00ff-7f0f-800f-0000000000ff"
	root, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:00:10.000Z","type":"event_msg","payload":{"type":"user_message","message":"poll pair"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	first, err := reader.readItems(context.Background(), rootID, threadID, "", "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.TailCursor == "" {
		t.Fatalf("initial forward page wrong: %+v", first)
	}

	// The second record family lands after the tail cursor was minted, then
	// a new dual-written message follows.
	appendRollout(t, path, strings.Join([]string{
		`{"timestamp":"2026-10-03T10:00:20.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"poll pair"}]}}`,
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"next question"}}`,
		`{"timestamp":"2026-10-03T10:01:00.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"next question"}]}}`,
	}, "\n")+"\n")

	second, err := reader.readItems(context.Background(), rootID, threadID, first.TailCursor, "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := itemKinds(second.Items)
	want := []string{"user_message"}
	if len(got) != len(want) {
		t.Fatalf("forward-poll twin replayed: %v", got)
	}
	if second.Items[0].Text != "next question" {
		t.Fatalf("new message lost to dedup: %+v", second.Items[0])
	}
}

// Two genuinely identical messages — same text, sent twice, far apart —
// must both survive even when the newer pair is split across a page seam.
func TestConversationReaderIdenticalMessagesFarApartBothSurvive(t *testing.T) {
	threadID := "019fc0c1-01aa-710a-810a-0000000001aa"
	lines := []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:00:10.000Z","type":"event_msg","payload":{"type":"user_message","message":"ok"}}`,
		`{"timestamp":"2026-10-03T10:00:10.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}}`,
	}
	// Ten emitted boundary records push the second pair well outside the
	// in-page dedup span.
	for i := 0; i < 10; i++ {
		lines = append(lines,
			fmt.Sprintf(`{"timestamp":"2026-10-03T10:01:%02d.000Z","type":"event_msg","payload":{"type":"warning","message":"w%d"}}`, i, i))
	}
	lines = append(lines,
		`{"timestamp":"2026-10-03T10:02:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"ok"}}`,
		`{"timestamp":"2026-10-03T10:02:00.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ok"}]}}`,
	)
	root, _ := writeReaderRollout(t, threadID, lines)
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	// The newest page holds only the response_item copy of the second "ok";
	// its event_msg twin is the last record of the older page.
	page, err := reader.readItems(context.Background(), rootID, threadID, "", "backward", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemKinds(page.Items); len(got) != 1 || got[0] != "user_message" {
		t.Fatalf("page 1 wrong: %v", got)
	}
	page2, err := reader.readItems(context.Background(), rootID, threadID, page.NextCursor, "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Page 2 must show the first "ok" exactly once (its own pair collapsed
	// in-page) while the seam twin of the second "ok" stays suppressed.
	userMessages := 0
	for _, item := range page2.Items {
		if item.Kind == "user_message" {
			userMessages++
			if item.Text != "ok" {
				t.Fatalf("unexpected user message: %+v", item)
			}
		}
	}
	if userMessages != 1 {
		t.Fatalf("expected the first identical message exactly once, got %d user messages in %v", userMessages, itemKinds(page2.Items))
	}
}

// Ring fingerprints are consumed on use: when the sibling twin claims the
// fingerprint, a genuinely repeated identical message inside the ring
// window still survives.
func TestConversationReaderRingFingerprintConsumedBySibling(t *testing.T) {
	threadID := "019fc0c1-01bb-710b-810b-0000000001bb"
	root, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:00:10.000Z","type":"event_msg","payload":{"type":"user_message","message":"dup"}}`,
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	first, err := reader.readItems(context.Background(), rootID, threadID, "", "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	appendRollout(t, path, strings.Join([]string{
		// The sibling twin of the polled message, then a genuinely repeated
		// identical message.
		`{"timestamp":"2026-10-03T10:00:20.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"dup"}]}}`,
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"dup"}}`,
	}, "\n")+"\n")

	second, err := reader.readItems(context.Background(), rootID, threadID, first.TailCursor, "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := itemKinds(second.Items)
	want := []string{"user_message"}
	if len(got) != len(want) || second.Items[0].Text != "dup" {
		t.Fatalf("repeated message inside the ring window must survive: %v", got)
	}
}

// The cursor version bumped when the dedup ring was added (v2) and again
// when positions generalized to a file set (v3): older cursors cannot
// describe the constituent files and are rejected as invalid so clients
// re-anchor at the latest page, and a malformed ring or file witness
// invalidates the token like any other structural corruption.
func TestConversationReaderRejectsLegacyAndMalformedCursors(t *testing.T) {
	v1 := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"h":"` + strings.Repeat("0", sha256.Size*2) + `","o":0,"s":10,"m":1}`))
	if _, err := decodeReaderCursor(v1); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("v1 cursor must be rejected: %v", err)
	}
	v2 := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":2,"h":"` + strings.Repeat("0", sha256.Size*2) + `","o":0,"s":10,"m":1}`))
	if _, err := decodeReaderCursor(v2); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("v2 cursor must be rejected: %v", err)
	}
	witness := `{"h":"` + strings.Repeat("0", sha256.Size*2) + `","s":10,"m":1}`
	missingSet := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":3,"f":0,"o":0}`))
	if _, err := decodeReaderCursor(missingSet); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("cursor without the file witness set must be rejected: %v", err)
	}
	outOfRange := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":3,"f":2,"o":0,"w":[` + witness + `]}`))
	if _, err := decodeReaderCursor(outOfRange); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("cursor pointing past its file set must be rejected: %v", err)
	}
	oversized := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":3,"f":0,"o":0,"w":[` + witness + `],"d":[` +
			strings.Repeat(`"0123456789abcdef",`, conversationMessageDedupSpan) + `"0123456789abcdef"]}`))
	if _, err := decodeReaderCursor(oversized); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("ring beyond the dedup span must be rejected: %v", err)
	}
	badFingerprint := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":3,"f":0,"o":0,"w":[` + witness + `],"d":["not-hex"]}`))
	if _, err := decodeReaderCursor(badFingerprint); !errors.Is(err, errConversationCursorInvalid) {
		t.Fatalf("non-fingerprint ring entry must be rejected: %v", err)
	}
}

func TestConversationReaderUnsupportedAndNotFound(t *testing.T) {
	reader, root := readerFixtureReader(t)
	rootID := conversationRootID(root)

	if _, err := reader.readItems(context.Background(), "rdeadbeefdeadbeef", readerThreadC1, "", "backward", 0); !errors.Is(err, errConversationNotFound) {
		t.Fatalf("unknown root: %v", err)
	}
	if _, err := reader.readItems(context.Background(), rootID, "../../etc/passwd", "", "backward", 0); !errors.Is(err, errConversationNotFound) {
		t.Fatalf("traversal thread id: %v", err)
	}
	if _, err := reader.readItems(context.Background(), rootID, catalogThreadB1, "", "backward", 0); !errors.Is(err, errConversationNotFound) {
		t.Fatalf("absent thread: %v", err)
	}

	// A resolved path whose persisted identity no longer matches is an
	// unsupported projection, not an empty page (post-resolve TOCTOU).
	threadID := "019fc0c1-00ee-7c0e-800e-0000000000ee"
	tmpRoot, path := writeReaderRollout(t, threadID, []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
	})
	resolved := resolvedConversation{
		RootID:   conversationRootID(tmpRoot),
		ThreadID: threadID,
		Root:     tmpRoot,
		Files:    []resolvedConversationFile{{Path: path}},
	}
	if err := os.WriteFile(path, []byte(
		`{"timestamp":"2026-10-03T10:00:00.000Z","type":"session_meta","payload":{"id":"019fc0c1-00ff-7c0f-800f-0000000000ff","source":"cli"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := reader.readResolvedItems(context.Background(), resolved, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.State != conversationStateUnsupported || len(page.Items) != 0 {
		t.Fatalf("meta mismatch should report unsupported: %+v", page)
	}
}

func TestConversationReaderArchivedAndSubagentPages(t *testing.T) {
	reader, root := readerFixtureReader(t)
	rootID := conversationRootID(root)

	archived, err := reader.readItems(context.Background(), rootID, readerThreadC2, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !archived.Archived {
		t.Fatal("archived conversation page missing archived flag")
	}
	if len(archived.Items) == 0 || archived.Items[0].Marker != conversationBoundarySession {
		t.Fatalf("archived page items wrong: %+v", archived.Items)
	}

	sub, err := reader.readItems(context.Background(), rootID, readerThreadC3, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Kind != "subagent" || sub.Archived {
		t.Fatalf("subagent page flags wrong: %+v", sub)
	}
	if len(sub.Items) == 0 || sub.Items[0].Kind != "boundary" || sub.Items[0].Marker != conversationBoundarySubagent {
		t.Fatalf("subagent boundary marker missing: %+v", sub.Items)
	}
}
