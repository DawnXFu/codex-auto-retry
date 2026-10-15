package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	catalogThreadA1 = "019fa0a1-0001-7a01-8001-0000000000a1"
	catalogThreadA2 = "019fa0a1-0002-7a02-8002-0000000000a2"
	catalogThreadA3 = "019fa0a1-0003-7a03-8003-0000000000a3"
	catalogThreadA4 = "019fa0a1-0004-7a04-8004-0000000000a4"
	catalogThreadB1 = "019fb0b1-0001-7b01-8001-0000000000b1"
)

func catalogFixtureRoots(t *testing.T) (homeA sessionRoot, homeB sessionRoot) {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("testdata", "conversations"))
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(base, "homeA")
	b := filepath.Join(base, "homeB")
	return sessionRoot{
		CodexHome:        a,
		Sessions:         filepath.Join(a, "sessions"),
		ArchivedSessions: filepath.Join(a, "archived_sessions"),
	}, sessionRoot{
		CodexHome: b,
		Sessions:  filepath.Join(b, "sessions"),
	}
}

func catalogIDs(items []conversationSummary) [][2]string {
	out := make([][2]string, 0, len(items))
	for _, item := range items {
		out = append(out, [2]string{item.RootID, item.ThreadID})
	}
	return out
}

func TestConversationCatalogListsOrdinaryConversationsByPersistedActivity(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	page, err := catalog.list(context.Background(), conversationFilterOrdinary, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	rootA := conversationRootID(homeA)
	rootB := conversationRootID(homeB)
	want := [][2]string{
		{rootB, catalogThreadB1}, // 2026-10-02T12:05
		{rootB, catalogThreadA1}, // 2026-10-01T09:00 mirrored copy
		{rootA, catalogThreadA1}, // 2026-10-01T08:05
	}
	got := catalogIDs(page.Items)
	if len(got) != len(want) {
		t.Fatalf("ordinary page items = %+v, want %+v", page.Items, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %v, want %v (full page %+v)", i, got[i], want[i], page.Items)
		}
	}
	if page.NextCursor != "" || page.Partial {
		t.Fatalf("unexpected pagination state cursor=%q partial=%v", page.NextCursor, page.Partial)
	}
	if page.Items[0].LastActive != "2026-10-02T12:05:00Z" {
		t.Fatalf("last_active = %q, want persisted record time", page.Items[0].LastActive)
	}
	if page.Items[2].Title != "Fix the login redirect" {
		t.Fatalf("title = %q", page.Items[2].Title)
	}
}

func TestConversationRootIDIsOpaqueAndStableAcrossOrdering(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	first := newConversationCatalog([]sessionRoot{homeA, homeB})
	second := newConversationCatalog([]sessionRoot{homeB, homeA})
	pageA, err := first.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	pageB, err := second.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pageA.Items) == 0 || len(pageA.Items) != len(pageB.Items) {
		t.Fatalf("listing changed with root order: %d vs %d", len(pageA.Items), len(pageB.Items))
	}
	for i := range pageA.Items {
		if pageA.Items[i].RootID != pageB.Items[i].RootID {
			t.Fatalf("root id changed with ordering at item %d", i)
		}
		if pageA.Items[i].RootID == "" || pageA.Items[i].RootID == pageA.Items[i].ThreadID {
			t.Fatalf("root id is not opaque: %q", pageA.Items[i].RootID)
		}
	}
	pattern := regexp.MustCompile(`^r[0-9a-f]{16}$`)
	for _, item := range pageA.Items {
		if !pattern.MatchString(item.RootID) {
			t.Fatalf("root id %q leaks path or is unstable format", item.RootID)
		}
	}
	for _, item := range pageA.Items {
		if filepath.IsAbs(item.RootID) || item.RootID == homeA.CodexHome || item.RootID == homeB.CodexHome {
			t.Fatalf("root id exposes filesystem path: %q", item.RootID)
		}
	}
}

func TestConversationCatalogFilters(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	rootA := conversationRootID(homeA)
	rootB := conversationRootID(homeB)

	page, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{rootB, catalogThreadB1},
		{rootB, catalogThreadA1},
		{rootA, catalogThreadA1},
		{rootA, catalogThreadA2},
		{rootA, catalogThreadA4},
		{rootA, catalogThreadA3},
	}
	if got := catalogIDs(page.Items); len(got) != len(want) {
		t.Fatalf("all page items = %+v, want %+v", page.Items, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("all page item %d = %v, want %v (full %+v)", i, got[i], want[i], page.Items)
			}
		}
	}

	page, err = catalog.list(context.Background(), conversationFilterSubagent, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantSub := [][2]string{{rootA, catalogThreadA2}, {rootA, catalogThreadA4}}
	if got := catalogIDs(page.Items); len(got) != len(wantSub) || got[0] != wantSub[0] || got[1] != wantSub[1] {
		t.Fatalf("subagent page items = %+v, want %+v", page.Items, wantSub)
	}
	for _, item := range page.Items {
		if item.Kind != "subagent" || item.Archived {
			t.Fatalf("subagent page returned %+v", item)
		}
	}

	page, err = catalog.list(context.Background(), conversationFilterArchived, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantArchived := [][2]string{{rootA, catalogThreadA3}}
	if got := catalogIDs(page.Items); len(got) != 1 || got[0] != wantArchived[0] {
		t.Fatalf("archived page items = %+v, want %+v", page.Items, wantArchived)
	}
	if !page.Items[0].Archived || page.Items[0].Kind != "ordinary" {
		t.Fatalf("archived entry flags wrong: %+v", page.Items[0])
	}
}

func TestConversationCatalogRejectsMismatchedAndMetaLessManifests(t *testing.T) {
	// homeB contains rollout-...-b2.jsonl whose session_meta id differs from the
	// filename, and rollout-...-b3.jsonl which has no session_meta at all. Both
	// must stay out of every view, including "all".
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ThreadID == "019fb0b1-0002-7b02-8002-0000000000b2" ||
			item.ThreadID == "019fb0b1-0003-7b03-8003-0000000000b3" {
			t.Fatalf("invalid manifest was listed: %+v", item)
		}
	}
}

func TestParseconversationFilter(t *testing.T) {
	for input, want := range map[string]conversationFilter{
		"":         conversationFilterOrdinary,
		"ordinary": conversationFilterOrdinary,
		"normal":   conversationFilterOrdinary,
		"all":      conversationFilterAll,
		"subagent": conversationFilterSubagent,
		"archived": conversationFilterArchived,
		" ALL ":    conversationFilterAll,
	} {
		got, err := parseconversationFilter(input)
		if err != nil || got != want {
			t.Fatalf("parseconversationFilter(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := parseconversationFilter("everything"); err == nil {
		t.Fatal("expected error for unknown filter")
	}
}

func TestConversationCatalogPaginationIsStableAndOpaque(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	full, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen [][2]string
	cursor := ""
	pages := 0
	for {
		page, err := catalog.list(context.Background(), conversationFilterAll, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page.Items) == 0 {
			t.Fatalf("empty page returned with cursor %q", cursor)
		}
		if len(page.Items) > 2 {
			t.Fatalf("page exceeded limit: %+v", page.Items)
		}
		seen = append(seen, catalogIDs(page.Items)...)
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages of 2, got %d", pages)
	}
	want := catalogIDs(full.Items)
	if len(seen) != len(want) {
		t.Fatalf("paginated union %v differs from full page %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("paginated order diverged at %d: %v vs %v", i, seen, want)
		}
	}
}

func TestConversationCatalogCursorRejectsReplayAcrossFilters(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	if _, err := catalog.list(context.Background(), conversationFilterSubagent, page.NextCursor, 2); err == nil {
		t.Fatal("cursor from filter=all was accepted under filter=subagent")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, "not-a-cursor", 2); err == nil {
		t.Fatal("garbage cursor accepted")
	}
	// A well-formed envelope with forged fields is still rejected.
	forged := encodeConversationCursor(conversationCursor{
		Filter: string(conversationFilterAll),
		Last:   "not-a-time",
		Root:   "r0000",
		Thread: catalogThreadA1,
	})
	if _, err := catalog.list(context.Background(), conversationFilterAll, forged, 2); err == nil {
		t.Fatal("cursor with invalid timestamp accepted")
	}
}

func TestConversationCatalogResolveHandsOffValidatedPath(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	resolved, err := catalog.resolve(conversationRootID(homeA), catalogThreadA1)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ThreadID != catalogThreadA1 || resolved.Kind != "ordinary" || resolved.Archived {
		t.Fatalf("resolved flags wrong: %+v", resolved)
	}
	if !catalogPathWithin(homeA.CodexHome, resolved.Path) {
		t.Fatalf("resolved path %q escaped root %q", resolved.Path, homeA.CodexHome)
	}
	wantSuffix := filepath.Join("homeA", "sessions", "2026", "10", "01",
		"rollout-2026-10-01T08-00-00-"+catalogThreadA1+".jsonl")
	if !strings.HasSuffix(resolved.Path, wantSuffix) {
		t.Fatalf("resolved path %q, want suffix %q", resolved.Path, wantSuffix)
	}

	archived, err := catalog.resolve(conversationRootID(homeA), catalogThreadA3)
	if err != nil {
		t.Fatal(err)
	}
	if !archived.Archived || !strings.Contains(archived.Path, "archived_sessions") {
		t.Fatalf("archived resolution wrong: %+v", archived)
	}

	if _, err := catalog.resolve("rdeadbeefdeadbeef", catalogThreadA1); err == nil {
		t.Fatal("unknown root id resolved")
	}
	if _, err := catalog.resolve(conversationRootID(homeA), "../../etc/passwd"); err == nil {
		t.Fatal("traversal thread id resolved")
	}
	if _, err := catalog.resolve(conversationRootID(homeA), "019fb0b1-0002-7b02-8002-0000000000b2"); err == nil {
		t.Fatal("meta-mismatched manifest resolved")
	}
	if _, err := catalog.resolve(conversationRootID(homeB), catalogThreadA4); err == nil {
		t.Fatal("thread absent from root resolved")
	}
}

func TestConversationCatalogResolvePrefersLiveOverArchivedCopy(t *testing.T) {
	home := t.TempDir()
	threadID := "019fc0c0-0001-7c01-8001-0000000000c1"
	sessions := filepath.Join(home, "sessions", "2026", "10", "01")
	archivedDir := filepath.Join(home, "archived_sessions", "2026", "08", "01")
	for _, dir := range []string{sessions, archivedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	meta := func(ts string) string {
		return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli"}}`
	}
	writeRollout(t, sessions, "rollout-2026-10-01T08-00-00-"+threadID+".jsonl", []string{
		meta("2026-10-01T08:00:00.000Z"),
		`{"timestamp":"2026-10-01T08:01:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	})
	writeRollout(t, archivedDir, "rollout-2026-08-01T08-00-00-"+threadID+".jsonl", []string{
		meta("2026-08-01T08:00:00.000Z"),
		`{"timestamp":"2026-08-01T09:00:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	})
	root := sessionRoot{CodexHome: home, Sessions: filepath.Join(home, "sessions"), ArchivedSessions: filepath.Join(home, "archived_sessions")}
	catalog := newConversationCatalog([]sessionRoot{root})

	page, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Archived {
		t.Fatalf("thread with live+archived copies listed as %+v", page.Items)
	}
	if page.Items[0].LastActive != "2026-10-01T08:01:00Z" {
		t.Fatalf("merged last_active = %q", page.Items[0].LastActive)
	}
	resolved, err := catalog.resolve(conversationRootID(root), threadID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Archived || !strings.Contains(resolved.Path, string(filepath.Separator)+"sessions"+string(filepath.Separator)) {
		t.Fatalf("resolve picked archived copy: %+v", resolved)
	}
}

func TestConversationCatalogSortIgnoresFileMtime(t *testing.T) {
	// File mtimes must never masquerade as message time: the manifest with the
	// newer persisted record wins even when its file is older on disk.
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRecords := "019fd0d0-0001-7d01-8001-0000000000d1"
	newRecords := "019fd0d0-0002-7d02-8002-0000000000d2"
	meta := func(id, ts string) string {
		return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"` + id + `","source":"cli"}}`
	}
	stalePath := writeRollout(t, sessions, "rollout-2026-10-01T08-00-00-"+oldRecords+".jsonl", []string{
		meta(oldRecords, "2026-10-01T08:00:00.000Z"),
		`{"timestamp":"2026-10-01T08:30:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	})
	freshPath := writeRollout(t, sessions, "rollout-2026-10-01T09-00-00-"+newRecords+".jsonl", []string{
		meta(newRecords, "2026-10-01T09:00:00.000Z"),
		`{"timestamp":"2026-10-01T09:30:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	})
	// Invert mtimes: the file with older records looks newer on disk.
	latest := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	earliest := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(stalePath, latest, latest); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshPath, earliest, earliest); err != nil {
		t.Fatal(err)
	}
	root := sessionRoot{CodexHome: home, Sessions: filepath.Join(home, "sessions")}
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ThreadID != newRecords || page.Items[1].ThreadID != oldRecords {
		t.Fatalf("ordering followed file mtime instead of persisted records: %+v", page.Items)
	}
}

func TestConversationCatalogIgnoresPartialFinalLine(t *testing.T) {
	// A record still being appended is not persisted yet, so its timestamp
	// must not count as conversation activity.
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "019fe0e0-0001-7e01-8001-0000000000e1"
	content := `{"timestamp":"2026-10-01T08:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli"}}` + "\n" +
		`{"timestamp":"2026-10-01T08:10:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant"}}` + "\n" +
		`{"timestamp":"2026-10-01T08:59:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"still wri`
	path := filepath.Join(sessions, "rollout-2026-10-01T08-00-00-"+threadID+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	root := sessionRoot{CodexHome: home, Sessions: filepath.Join(home, "sessions")}
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterOrdinary, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].LastActive != "2026-10-01T08:10:00Z" {
		t.Fatalf("partial final line leaked into activity time: %+v", page.Items)
	}
}

func TestConversationCatalogScanBoundMarksPartial(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	defer func(limit int) { maxCatalogScanFiles = limit }(maxCatalogScanFiles)
	maxCatalogScanFiles = 2
	page, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Partial {
		t.Fatal("scan past the file bound was not reported partial")
	}
	if len(page.Items) > 2 {
		t.Fatalf("scan exceeded file bound: %+v", page.Items)
	}
}

func TestConversationCatalogListHonorsContextCancel(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.list(ctx, conversationFilterAll, "", 0); err == nil {
		t.Fatal("canceled context did not abort the list")
	}
}

func TestConversationCatalogRejectsSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "019ff0f0-0001-7f01-8001-0000000000f1"
	target := filepath.Join(outside, "secret-"+threadID+".jsonl")
	if err := os.WriteFile(target, []byte(
		`{"timestamp":"2026-10-01T08:00:00.000Z","type":"session_meta","payload":{"id":"`+threadID+`","source":"cli"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sessions, "rollout-2026-10-01T08-00-00-"+threadID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	root := sessionRoot{CodexHome: home, Sessions: sessions}
	catalog := newConversationCatalog([]sessionRoot{root})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("symlinked file outside root was listed: %+v", page.Items)
	}
	if _, err := catalog.resolve(conversationRootID(root), threadID); err == nil {
		t.Fatal("symlinked file outside root resolved")
	}
}
