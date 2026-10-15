package watchdog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Multi-segment fixture: one thread resumed across several rollout files. The
// catalog must hand every whitelisted segment to the reader, ordered by first
// persisted record timestamp — never by directory — and the reader projects
// the set as one virtual concatenated log.

const multiFileThread = "019fc0c1-02aa-720a-820a-0000000002aa"

// writeThreadSegments writes rollout files for one thread into a temp home.
// Each segment is (archived, filename, lines); the helper returns the root and
// the written paths in declaration order.
func writeThreadSegments(t *testing.T, threadID string, segments []struct {
	Archived bool
	Name     string
	Lines    []string
}) (sessionRoot, []string) {
	t.Helper()
	home := t.TempDir()
	var paths []string
	for _, seg := range segments {
		base := filepath.Join(home, "sessions")
		if seg.Archived {
			base = filepath.Join(home, "archived_sessions")
		}
		dir := filepath.Join(base, "2026", "10", "03")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, writeRollout(t, dir, seg.Name, seg.Lines))
	}
	return sessionRoot{
		CodexHome:        home,
		Sessions:         filepath.Join(home, "sessions"),
		ArchivedSessions: filepath.Join(home, "archived_sessions"),
	}, paths
}

func userMsgLine(ts, text string) string {
	return `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"user_message","message":"` + text + `"}}`
}

func agentItemLine(ts, text string) string {
	return `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`
}

func pageTexts(page conversationItemPage) []string {
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.Kind+":"+item.Text)
	}
	return out
}

// resolveAll must collect every whitelisted segment carrying the thread's
// session_meta and order them by first persisted record timestamp — an older
// archived segment sorts before a newer live one even though it lives in
// archived_sessions.
func TestConversationCatalogResolveAllOrdersSegmentsByRecordTime(t *testing.T) {
	root, paths := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{true, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "old question"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "new question"),
		}},
	})
	catalog := newConversationCatalog([]sessionRoot{root})

	resolved, err := catalog.resolveAll(conversationRootID(root), multiFileThread)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Files) != 2 {
		t.Fatalf("resolveAll returned %d files, want 2", len(resolved.Files))
	}
	if resolved.Files[0].Path != paths[0] || resolved.Files[1].Path != paths[1] {
		t.Fatalf("segment order = %v, want record-time order [archived-copy, live-copy]",
			[]string{resolved.Files[0].Path, resolved.Files[1].Path})
	}
	// Mixed archive state: the conversation is not archived unless every
	// segment is.
	if resolved.Archived {
		t.Fatal("mixed live+archived segments marked archived")
	}
	if resolved.Kind != conversationKindOrdinary {
		t.Fatalf("kind = %q", resolved.Kind)
	}
}

// A thread fully moved into archived_sessions keeps archived=true only when
// every constituent segment is archived.
func TestConversationCatalogResolveAllAllArchived(t *testing.T) {
	root, _ := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{true, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "old question"),
		}},
		{true, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "new question"),
		}},
	})
	catalog := newConversationCatalog([]sessionRoot{root})
	resolved, err := catalog.resolveAll(conversationRootID(root), multiFileThread)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Archived {
		t.Fatal("all-archived segment set lost the archived flag")
	}
}

// The reader treats the resolved set as one concatenated log: a backward page
// starting at the newest file's tail walks seamlessly into the older segment.
func TestConversationReaderStitchesSegmentsBackward(t *testing.T) {
	root, _ := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{true, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "first segment question"),
			agentItemLine("2026-10-03T09:02:00.000Z", "first segment answer"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "second segment question"),
			agentItemLine("2026-10-03T11:02:00.000Z", "second segment answer"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))

	page, err := reader.readItems(context.Background(), conversationRootID(root), multiFileThread, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"boundary:",
		"user_message:first segment question",
		"assistant_message:first segment answer",
		"boundary:",
		"user_message:second segment question",
		"assistant_message:second segment answer",
	}
	got := pageTexts(page)
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	if page.Archived {
		t.Fatal("mixed segment set marked archived")
	}
	if page.NextCursor != "" {
		t.Fatal("full read still offers an older page")
	}
	if page.TailCursor == "" {
		t.Fatal("tail cursor missing")
	}
}

// Backward pagination must carry the seam across the file boundary: the
// paginated union reconstructs the single-page projection exactly.
func TestConversationReaderPaginatesAcrossSegmentBoundary(t *testing.T) {
	root, _ := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "q one"),
			userMsgLine("2026-10-03T09:02:00.000Z", "q two"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "q three"),
			userMsgLine("2026-10-03T11:02:00.000Z", "q four"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	full, err := reader.readItems(context.Background(), rootID, multiFileThread, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen []conversationItem
	var pages [][]conversationItem
	cursor := ""
	for i := 0; ; i++ {
		page, err := reader.readItems(context.Background(), rootID, multiFileThread, cursor, "backward", 3)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, page.Items)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if i > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	for i := len(pages) - 1; i >= 0; i-- {
		seen = append(seen, pages[i]...)
	}
	if len(seen) != len(full.Items) {
		t.Fatalf("paginated union %d items vs full %d", len(seen), len(full.Items))
	}
	for i := range full.Items {
		if seen[i].ID != full.Items[i].ID || seen[i].Text != full.Items[i].Text {
			t.Fatalf("item %d diverged: %+v vs %+v", i, seen[i], full.Items[i])
		}
	}
}

// A resumed rollout can begin by re-persisting records already present in the
// earlier segment (copied prefix). Identical records must render once — the
// earlier segment's copy wins.
func TestConversationReaderSuppressesCopiedPrefixRecords(t *testing.T) {
	copiedUser := userMsgLine("2026-10-03T09:02:00.000Z", "carried over question")
	copiedAgent := agentItemLine("2026-10-03T09:03:00.000Z", "carried over answer")
	root, _ := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "opening question"),
			copiedUser,
			copiedAgent,
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			copiedUser,
			copiedAgent,
			userMsgLine("2026-10-03T11:01:00.000Z", "fresh question"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))

	page, err := reader.readItems(context.Background(), conversationRootID(root), multiFileThread, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"boundary:",
		"user_message:opening question",
		"user_message:carried over question",
		"assistant_message:carried over answer",
		"boundary:",
		"user_message:fresh question",
	}
	got := pageTexts(page)
	if len(got) != len(want) {
		t.Fatalf("copied records double-rendered: %v (want %v)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The cursor fingerprints the whole constituent set: mutating ANY segment —
// not just the file the cursor's offset points into — invalidates it.
func TestConversationReaderCursorInvalidatesOnAnySegmentChange(t *testing.T) {
	root, paths := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "old one"),
			userMsgLine("2026-10-03T09:02:00.000Z", "old two"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "new one"),
			userMsgLine("2026-10-03T11:02:00.000Z", "new two"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, multiFileThread, "", "backward", 3)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" || page.TailCursor == "" {
		t.Fatal("expected cursors to invalidate")
	}

	// Rewrite the OLDER segment — the file the issued cursors do not point
	// into. Both continuations must fail with file-changed.
	if err := os.WriteFile(paths[0], []byte(
		readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readItems(context.Background(), rootID, multiFileThread, page.NextCursor, "backward", 3); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("older-segment truncation did not invalidate cursor: %v", err)
	}
	if _, err := reader.readItems(context.Background(), rootID, multiFileThread, page.TailCursor, "forward", 3); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("older-segment truncation did not invalidate tail cursor: %v", err)
	}
}

// A brand-new third segment appearing after cursors were minted also changes
// the constituent set — clients re-anchor.
func TestConversationReaderCursorInvalidatesOnSegmentAdded(t *testing.T) {
	segments := []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "first"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "second"),
		}},
	}
	root, _ := writeThreadSegments(t, multiFileThread, segments)
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, multiFileThread, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root.Sessions, "2026", "10", "03")
	writeRollout(t, dir, "rollout-2026-10-03T12-00-00-"+multiFileThread+".jsonl", []string{
		readerMetaLine(multiFileThread, "2026-10-03T12:00:00.000Z"),
		userMsgLine("2026-10-03T12:01:00.000Z", "third segment"),
	})
	if _, err := reader.readItems(context.Background(), rootID, multiFileThread, page.TailCursor, "forward", 0); !errors.Is(err, errConversationFileChanged) {
		t.Fatalf("new segment did not invalidate cursor: %v", err)
	}
}

// Forward polling follows the virtual log from an older segment into the
// newer one without a page seam.
func TestConversationReaderForwardScanCrossesSegments(t *testing.T) {
	root, _ := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "early"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "late"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, multiFileThread, "", "forward", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"boundary:",
		"user_message:early",
		"boundary:",
		"user_message:late",
	}
	got := pageTexts(page)
	if len(got) != len(want) {
		t.Fatalf("forward stitch = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	if page.TailCursor == "" {
		t.Fatal("tail cursor missing")
	}
}

// Appends to the newest segment keep forward cursors alive — the tail file's
// size/mtime are allowed to grow like the single-file reader.
func TestConversationReaderForwardFollowsAppendsInLastSegment(t *testing.T) {
	root, paths := writeThreadSegments(t, multiFileThread, []struct {
		Archived bool
		Name     string
		Lines    []string
	}{
		{false, "rollout-2026-10-03T09-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T09:00:00.000Z"),
			userMsgLine("2026-10-03T09:01:00.000Z", "early"),
		}},
		{false, "rollout-2026-10-03T11-00-00-" + multiFileThread + ".jsonl", []string{
			readerMetaLine(multiFileThread, "2026-10-03T11:00:00.000Z"),
			userMsgLine("2026-10-03T11:01:00.000Z", "late"),
		}},
	})
	reader := newConversationReader(newConversationCatalog([]sessionRoot{root}))
	rootID := conversationRootID(root)

	page, err := reader.readItems(context.Background(), rootID, multiFileThread, "", "backward", 0)
	if err != nil {
		t.Fatal(err)
	}
	appendRollout(t, paths[1], userMsgLine("2026-10-03T11:05:00.000Z", "appended")+"\n")
	follow, err := reader.readItems(context.Background(), rootID, multiFileThread, page.TailCursor, "forward", 0)
	if err != nil {
		t.Fatalf("append to tail segment broke the forward cursor: %v", err)
	}
	if len(follow.Items) != 1 || follow.Items[0].Text != "appended" {
		t.Fatalf("appended record not picked up: %+v", follow.Items)
	}
}
