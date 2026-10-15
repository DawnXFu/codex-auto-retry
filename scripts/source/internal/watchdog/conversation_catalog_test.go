package watchdog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	catalogThreadD1 = "019fd0d1-0001-7d01-8001-0000000000d1"
	catalogThreadD2 = "019fd0d1-0002-7d02-8002-0000000000d2"
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

// catalogFixtureRootD points at the homeD fixtures: real-machine shaped
// rollouts whose first persisted user message carries Codex's injected
// AGENTS.md/environment boilerplate instead of the typed prompt.
func catalogFixtureRootD(t *testing.T) sessionRoot {
	t.Helper()
	base, err := filepath.Abs(filepath.Join("testdata", "conversations", "homeD"))
	if err != nil {
		t.Fatal(err)
	}
	return sessionRoot{CodexHome: base, Sessions: filepath.Join(base, "sessions")}
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
	page, err := catalog.list(context.Background(), conversationFilterOrdinary, "", "", "", 0)
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
	pageA, err := first.list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	pageB, err := second.list(context.Background(), conversationFilterAll, "", "", "", 0)
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

	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
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

	page, err = catalog.list(context.Background(), conversationFilterSubagent, "", "", "", 0)
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

	page, err = catalog.list(context.Background(), conversationFilterArchived, "", "", "", 0)
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
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
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

func TestParseConversationFilter(t *testing.T) {
	for input, want := range map[string]conversationFilter{
		"":         conversationFilterOrdinary,
		"ordinary": conversationFilterOrdinary,
		"normal":   conversationFilterOrdinary,
		"all":      conversationFilterAll,
		"subagent": conversationFilterSubagent,
		"archived": conversationFilterArchived,
		" ALL ":    conversationFilterAll,
	} {
		got, err := parseConversationFilter(input)
		if err != nil || got != want {
			t.Fatalf("parseConversationFilter(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := parseConversationFilter("everything"); err == nil {
		t.Fatal("expected error for unknown filter")
	}
}

func TestConversationCatalogPaginationIsStableAndOpaque(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	full, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen [][2]string
	cursor := ""
	pages := 0
	for {
		page, err := catalog.list(context.Background(), conversationFilterAll, "", "", cursor, 2)
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
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	if _, err := catalog.list(context.Background(), conversationFilterSubagent, "", "", page.NextCursor, 2); err == nil {
		t.Fatal("cursor from filter=all was accepted under filter=subagent")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, "", "", "not-a-cursor", 2); err == nil {
		t.Fatal("garbage cursor accepted")
	}
	// A well-formed envelope with forged fields is still rejected.
	forged := encodeConversationCursor(conversationCursor{
		Filter: string(conversationFilterAll),
		Last:   "not-a-time",
		Root:   "r0000",
		Thread: catalogThreadA1,
	})
	if _, err := catalog.list(context.Background(), conversationFilterAll, "", "", forged, 2); err == nil {
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
	if len(resolved.Files) != 1 {
		t.Fatalf("resolve returned %d files, want the single preferred file", len(resolved.Files))
	}
	if !catalogPathWithin(homeA.CodexHome, resolved.Files[0].Path) {
		t.Fatalf("resolved path %q escaped root %q", resolved.Files[0].Path, homeA.CodexHome)
	}
	wantSuffix := filepath.Join("homeA", "sessions", "2026", "10", "01",
		"rollout-2026-10-01T08-00-00-"+catalogThreadA1+".jsonl")
	if !strings.HasSuffix(resolved.Files[0].Path, wantSuffix) {
		t.Fatalf("resolved path %q, want suffix %q", resolved.Files[0].Path, wantSuffix)
	}

	archived, err := catalog.resolve(conversationRootID(homeA), catalogThreadA3)
	if err != nil {
		t.Fatal(err)
	}
	if !archived.Archived || !strings.Contains(archived.Files[0].Path, "archived_sessions") {
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

	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
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
	if resolved.Archived || len(resolved.Files) != 1 ||
		!strings.Contains(resolved.Files[0].Path, string(filepath.Separator)+"sessions"+string(filepath.Separator)) ||
		strings.Contains(resolved.Files[0].Path, "archived_sessions") {
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
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterAll, "", "", "", 0)
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
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterOrdinary, "", "", "", 0)
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
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
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
	if _, err := catalog.list(ctx, conversationFilterAll, "", "", "", 0); err == nil {
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
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
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

// Real-machine rollouts inject AGENTS.md/environment boilerplate into the
// first persisted user message; the derived title must be the text the user
// actually typed, never the injection header.
func TestConversationCatalogTitleSkipsAgentsInjection(t *testing.T) {
	rootD := catalogFixtureRootD(t)
	catalog := newConversationCatalog([]sessionRoot{rootD})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	for _, item := range page.Items {
		titles[item.ThreadID] = item.Title
		if strings.Contains(item.Title, "AGENTS.md") || strings.HasPrefix(item.Title, "#") {
			t.Fatalf("injected boilerplate leaked into title: %q", item.Title)
		}
	}
	if titles[catalogThreadD1] != "Investigate the flaky retry poll" {
		t.Fatalf("d1 title = %q, want the typed prompt past the injected block", titles[catalogThreadD1])
	}
}

// A persisted thread_name_updated record is an explicit title signal: it
// outranks text derived from user messages, even when the user text itself
// survived boilerplate stripping.
func TestConversationCatalogTitlePrefersRenameRecord(t *testing.T) {
	rootD := catalogFixtureRootD(t)
	catalog := newConversationCatalog([]sessionRoot{rootD})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ThreadID == catalogThreadD2 {
			if item.Title != "Polish quota card" {
				t.Fatalf("d2 title = %q, want the persisted rename", item.Title)
			}
			return
		}
	}
	t.Fatal("d2 missing from listing")
}

// A rename record persisted late in a large rollout still wins: the bounded
// tail scan covers the region the head window cannot reach.
func TestConversationCatalogTitleFindsLateRename(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions", "2026", "10", "05")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "019fd0d1-0003-7d03-8003-0000000000d3"
	lines := []string{
		`{"timestamp":"2026-10-05T08:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli"}}`,
		`{"timestamp":"2026-10-05T08:00:05.000Z","type":"event_msg","payload":{"type":"user_message","message":"Triage the quota regression"}}`,
	}
	// Push the rename past both the head line cap and the head byte window:
	// it only exists inside the tail scan region.
	filler := `{"timestamp":"2026-10-05T09:00:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` +
		strings.Repeat("x", 4096) + `"}]}}`
	for i := 0; i < int(maxCatalogHeadBytes/4096)+16; i++ {
		lines = append(lines, filler)
	}
	lines = append(lines,
		`{"timestamp":"2026-10-05T10:00:00.000Z","type":"event_msg","payload":{"type":"thread_name_updated","name":"Quota triage renamed"}}`,
		`{"timestamp":"2026-10-05T10:01:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}}`,
	)
	writeRollout(t, sessions, "rollout-2026-10-05T08-00-00-"+threadID+".jsonl", lines)
	root := sessionRoot{CodexHome: home, Sessions: filepath.Join(home, "sessions")}
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "Quota triage renamed" {
		t.Fatalf("late rename not picked up: %+v", page.Items)
	}
}

// The session_meta payload cwd rides on each summary so clients can show
// which project directory a conversation ran in; a record without one keeps
// the field empty rather than guessing.
func TestConversationCatalogCapturesWorkingDirectory(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		want := `C:\repo`
		if item.ThreadID == catalogThreadB1 {
			want = `C:\repo2`
		}
		if item.Cwd != want {
			t.Fatalf("item %s cwd = %q, want %q", item.ThreadID, item.Cwd, want)
		}
	}

	// A session_meta without a cwd leaves the field empty — never fabricated.
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "019fe0e0-0002-7e02-8002-0000000000e2"
	writeRollout(t, sessions, "rollout-2026-10-06T08-00-00-"+threadID+".jsonl", []string{
		`{"timestamp":"2026-10-06T08:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","source":"cli"}}`,
	})
	bare := sessionRoot{CodexHome: home, Sessions: sessions}
	barePage, err := newConversationCatalog([]sessionRoot{bare}).list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(barePage.Items) != 1 || barePage.Items[0].Cwd != "" {
		t.Fatalf("meta without cwd produced %+v", barePage.Items)
	}
}

// dir= narrows the listing to conversations recorded in that working
// directory. It is a filter, not an identity: an unmatched value returns an
// empty page, never an error. Matching is an exact compare after
// clean+case-fold normalization (case-insensitive only where the platform
// filesystem is).
func TestConversationCatalogFiltersByDirectory(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	page, err := catalog.list(context.Background(), conversationFilterAll, "", `C:\repo`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("dir-scoped items = %+v, want the 5 C:\\repo conversations", page.Items)
	}
	for _, item := range page.Items {
		if item.Cwd != `C:\repo` {
			t.Fatalf("dir filter leaked item with cwd %q", item.Cwd)
		}
	}

	page, err = catalog.list(context.Background(), conversationFilterAll, "", `C:\repo2`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ThreadID != catalogThreadB1 {
		t.Fatalf("dir C:\\repo2 items = %+v, want b1 only", page.Items)
	}

	page, err = catalog.list(context.Background(), conversationFilterAll, "", `D:\no-such-project`, "", 0)
	if err != nil {
		t.Fatalf("unknown dir is a filter miss, not an error: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("unknown dir matched %+v", page.Items)
	}

	// Case folding follows the platform filesystem, matching
	// catalogPathWithin's convention.
	folded, err := catalog.list(context.Background(), conversationFilterAll, "", `c:\REPO`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if matched := len(folded.Items) == 5; matched != conversationPathCaseInsensitive {
		t.Fatalf("case-folded dir match = %d items, want match=%v on this platform",
			len(folded.Items), conversationPathCaseInsensitive)
	}
}

// Every page reports the distinct working directories present across the
// filtered scan scope — computed before pagination and regardless of the
// dir selection itself, so a client's dropdown stays complete and stable.
// Labels are the dir basename, parent-prefixed on collision like root labels.
func TestConversationCatalogListReportsDirs(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor for limit=1")
	}
	if len(page.Dirs) != 2 {
		t.Fatalf("dirs = %+v, want the two recorded cwds", page.Dirs)
	}
	labels := map[string]string{}
	for _, ref := range page.Dirs {
		if ref.Dir == "" || ref.Label == "" {
			t.Fatalf("dir ref incomplete: %+v", ref)
		}
		labels[ref.Dir] = ref.Label
	}
	if labels[`C:\repo`] != "repo" || labels[`C:\repo2`] != "repo2" {
		t.Fatalf("dir labels = %+v, want dir basenames", page.Dirs)
	}

	// The selected dir must not shrink the dropdown scope, and a later page
	// reports the same set.
	scoped, err := catalog.list(context.Background(), conversationFilterAll, "", `C:\repo2`, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Dirs) != 2 {
		t.Fatalf("dir-scoped dirs = %+v, want the full scope set", scoped.Dirs)
	}
	next, err := catalog.list(context.Background(), conversationFilterAll, "", "", page.NextCursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Dirs) != len(page.Dirs) {
		t.Fatalf("page 2 dirs = %+v, want stable %+v", next.Dirs, page.Dirs)
	}
	for i := range page.Dirs {
		if next.Dirs[i] != page.Dirs[i] {
			t.Fatalf("page 2 dirs diverged: %+v vs %+v", next.Dirs, page.Dirs)
		}
	}
}

// Basename collisions between recorded working directories disambiguate with
// the parent segment, the same convention as monitored-root labels.
func TestConversationDirLabelsDisambiguateDuplicates(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	threads := []struct{ id, cwd string }{
		{"019fe0e0-0003-7e03-8003-0000000000e3", filepath.Join(base, "one", "proj")},
		{"019fe0e0-0004-7e04-8004-0000000000e4", filepath.Join(base, "two", "proj")},
		{"019fe0e0-0005-7e05-8005-0000000000e5", filepath.Join(base, "three", "solo")},
	}
	for i, tc := range threads {
		writeRollout(t, sessions, fmt.Sprintf("rollout-2026-10-06T08-0%d-00-%s.jsonl", i, tc.id), []string{
			`{"timestamp":"2026-10-06T08:00:00.000Z","type":"session_meta","payload":{"id":"` + tc.id +
				`","source":"cli","cwd":` + strconv.Quote(tc.cwd) + `}}`,
		})
	}
	root := sessionRoot{CodexHome: home, Sessions: sessions}
	page, err := newConversationCatalog([]sessionRoot{root}).list(context.Background(), conversationFilterAll, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, ref := range page.Dirs {
		labels[ref.Dir] = ref.Label
	}
	sep := "/"
	want := map[string]string{
		threads[0].cwd: "one" + sep + "proj",
		threads[1].cwd: "two" + sep + "proj",
		threads[2].cwd: "solo",
	}
	for dir, label := range want {
		if labels[dir] != label {
			t.Fatalf("dir %q label = %q, want %q (all %+v)", dir, labels[dir], label, page.Dirs)
		}
	}
}

// A cursor is bound to the dir scope that issued it, exactly like the filter
// and root bindings: replay under a different dir is a client error.
func TestConversationCatalogCursorBindsToDir(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})

	page, err := catalog.list(context.Background(), conversationFilterAll, "", `C:\repo`, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, "", `C:\repo2`, page.NextCursor, 1); err == nil {
		t.Fatal("C:\\repo cursor accepted under C:\\repo2")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, "", "", page.NextCursor, 1); err == nil {
		t.Fatal("dir-scoped cursor accepted without the dir")
	}
	// Same dir replay continues fine.
	next, err := catalog.list(context.Background(), conversationFilterAll, "", `C:\repo`, page.NextCursor, 10)
	if err != nil {
		t.Fatalf("same-dir cursor replay failed: %v", err)
	}
	if len(next.Items) != 4 {
		t.Fatalf("continuation items = %+v, want the remaining 4", next.Items)
	}
}

// root= scopes the listing to one monitored root; an unknown root id is a
// client error, never an empty page.
func TestConversationCatalogFiltersByRoot(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	rootA := conversationRootID(homeA)
	rootB := conversationRootID(homeB)

	page, err := catalog.list(context.Background(), conversationFilterAll, rootA, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 {
		t.Fatalf("rootA page items = %+v, want 4 entries", page.Items)
	}
	for _, item := range page.Items {
		if item.RootID != rootA {
			t.Fatalf("root filter leaked item from %q", item.RootID)
		}
	}

	page, err = catalog.list(context.Background(), conversationFilterAll, rootB, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("rootB page items = %+v, want 2 entries", page.Items)
	}
	for _, item := range page.Items {
		if item.RootID != rootB {
			t.Fatalf("root filter leaked item from %q", item.RootID)
		}
	}

	if _, err := catalog.list(context.Background(), conversationFilterAll, "rdeadbeefdeadbeef", "", "", 0); err == nil {
		t.Fatal("unknown root id listed instead of rejected")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, "../../etc", "", "", 0); err == nil {
		t.Fatal("path-shaped root id listed instead of rejected")
	}
}

// Every page reports the monitored roots so the client's filter list stays
// stable across filter/root changes — including roots whose conversations
// the current view excludes.
func TestConversationCatalogListReportsRoots(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	rootA := conversationRootID(homeA)
	rootB := conversationRootID(homeB)

	page, err := catalog.list(context.Background(), conversationFilterArchived, rootA, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Roots) != 2 {
		t.Fatalf("roots = %+v, want both monitored roots", page.Roots)
	}
	seen := map[string]string{}
	for _, ref := range page.Roots {
		if ref.Label == "" {
			t.Fatalf("root %q has no label", ref.RootID)
		}
		seen[ref.RootID] = ref.Label
	}
	if seen[rootA] == "" || seen[rootB] == "" {
		t.Fatalf("roots missing expected ids: %+v", page.Roots)
	}
	for _, ref := range page.Roots {
		if filepath.IsAbs(ref.Label) || strings.ContainsAny(ref.Label, `:\`) {
			t.Fatalf("label leaks filesystem path detail: %q", ref.Label)
		}
	}
}

// Root labels are compact display text — the home basename, extended with a
// parent segment only when two monitored roots would otherwise collide.
func TestConversationRootLabelsDisambiguateDuplicates(t *testing.T) {
	base := t.TempDir()
	first := sessionRoot{CodexHome: filepath.Join(base, "one", ".codex")}
	second := sessionRoot{CodexHome: filepath.Join(base, "two", ".codex")}
	third := sessionRoot{CodexHome: filepath.Join(base, "three", "work-home")}
	labels := newConversationCatalog([]sessionRoot{first, second, third}).rootLabels()
	if len(labels) != 3 {
		t.Fatalf("labels = %+v", labels)
	}
	byID := map[string]string{}
	for _, l := range labels {
		byID[l.RootID] = l.Label
	}
	if byID[conversationRootID(first)] != "one/.codex" && byID[conversationRootID(first)] != "one"+string(filepath.Separator)+".codex" {
		t.Fatalf("duplicate basename not disambiguated: %q", byID[conversationRootID(first)])
	}
	if byID[conversationRootID(second)] != "two/.codex" && byID[conversationRootID(second)] != "two"+string(filepath.Separator)+".codex" {
		t.Fatalf("duplicate basename not disambiguated: %q", byID[conversationRootID(second)])
	}
	if byID[conversationRootID(third)] != "work-home" {
		t.Fatalf("unique basename gained noise: %q", byID[conversationRootID(third)])
	}
}

// A cursor is bound to the root scope that issued it, exactly like the
// filter binding: replay under a different root is a client error.
func TestConversationCatalogCursorBindsToRoot(t *testing.T) {
	homeA, homeB := catalogFixtureRoots(t)
	catalog := newConversationCatalog([]sessionRoot{homeA, homeB})
	rootA := conversationRootID(homeA)
	rootB := conversationRootID(homeB)

	page, err := catalog.list(context.Background(), conversationFilterAll, "", "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("expected a next cursor")
	}
	// Unscoped cursor replayed under a root scope → reject.
	if _, err := catalog.list(context.Background(), conversationFilterAll, rootA, "", page.NextCursor, 2); err == nil {
		t.Fatal("unscoped cursor accepted under a root scope")
	}
	// Root-scoped cursor replayed under a different root → reject.
	scoped, err := catalog.list(context.Background(), conversationFilterAll, rootA, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if scoped.NextCursor == "" {
		t.Fatal("expected a scoped next cursor")
	}
	if _, err := catalog.list(context.Background(), conversationFilterAll, rootB, "", scoped.NextCursor, 1); err == nil {
		t.Fatal("rootA cursor accepted under rootB")
	}
	// Same root replay is still fine and continues the page.
	next, err := catalog.list(context.Background(), conversationFilterAll, rootA, "", scoped.NextCursor, 10)
	if err != nil {
		t.Fatalf("same-root cursor replay failed: %v", err)
	}
	if len(next.Items) != 3 {
		t.Fatalf("continuation items = %+v, want the remaining 3", next.Items)
	}
}
