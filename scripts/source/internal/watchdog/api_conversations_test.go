package watchdog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// conversationsAPIRequest mirrors apiRequest but also returns the response
// headers so tests can assert Cache-Control on every response class.
func conversationsAPIRequest(t *testing.T, descriptor apiDescriptor, path, token string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", descriptor.Port, path), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response.StatusCode, response.Header, body
}

// configureConversationsRoots pins the management service at the fixture codex
// homes so discovery stays hermetic: no default home, no cockpit glob, only the
// listed session_roots.
func configureConversationsRoots(t *testing.T, dataDir string, homes ...string) {
	t.Helper()
	config := defaultConfig()
	config.IncludeDefaultHome = false
	config.IncludeCockpitHomes = false
	for _, home := range homes {
		abs, err := filepath.Abs(home)
		if err != nil {
			t.Fatalf("abs fixture home: %v", err)
		}
		config.SessionRoots = append(config.SessionRoots, abs)
	}
	if err := writeJSONAtomic(filepath.Join(dataDir, "config.json"), config); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func conversationsFixtureHomes(t *testing.T) (homeA, homeB string) {
	t.Helper()
	return filepath.Join("testdata", "conversations", "homeA"),
		filepath.Join("testdata", "conversations", "homeB")
}

type conversationsListBody struct {
	Items []struct {
		RootID       string `json:"root_id"`
		ThreadID     string `json:"thread_id"`
		Kind         string `json:"kind"`
		Archived     bool   `json:"archived"`
		Title        string `json:"title"`
		StartedAt    string `json:"started_at"`
		LastActivity string `json:"last_activity"`
	} `json:"items"`
	Roots []struct {
		RootID string `json:"root_id"`
		Label  string `json:"label"`
	} `json:"roots"`
	NextCursor string `json:"next_cursor"`
	Partial    bool   `json:"partial"`
	State      string `json:"state"`
}

func conversationsAPIList(t *testing.T, descriptor apiDescriptor, query string) conversationsListBody {
	t.Helper()
	status, header, body := conversationsAPIRequest(t, descriptor, "/conversations"+query, descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("GET /conversations%s status = %d body=%s", query, status, body)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var page conversationsListBody
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("list body is not JSON: %v body=%s", err, body)
	}
	return page
}

func TestConversationsAPIRequiresBearerAndNoStore(t *testing.T) {
	descriptor, _ := startTestAPI(t)

	status, header, body := conversationsAPIRequest(t, descriptor, "/conversations", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no token status = %d body=%s", status, body)
	}
	if header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q", header.Get("WWW-Authenticate"))
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("401 response Cache-Control = %q, want no-store", got)
	}
	if status, _, _ = conversationsAPIRequest(t, descriptor, "/conversations", "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", status)
	}
}

func TestConversationsAPIListDefaultsToOrdinary(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	homeA, homeB := conversationsFixtureHomes(t)
	configureConversationsRoots(t, dataDir, homeA, homeB)

	page := conversationsAPIList(t, descriptor, "")
	if page.State != "complete_persisted" || page.Partial {
		t.Fatalf("state=%q partial=%v, want complete_persisted/false", page.State, page.Partial)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected next_cursor %q", page.NextCursor)
	}
	wantThreads := []string{
		"019fb0b1-0001-7b01-8001-0000000000b1", // homeB, latest persisted activity
		"019fa0a1-0001-7a01-8001-0000000000a1", // homeB mirrored copy
		"019fa0a1-0001-7a01-8001-0000000000a1", // homeA copy
	}
	if len(page.Items) != len(wantThreads) {
		t.Fatalf("items = %+v, want %d entries", page.Items, len(wantThreads))
	}
	rootPattern := regexp.MustCompile(`^r[0-9a-f]{16}$`)
	for i, item := range page.Items {
		if item.ThreadID != wantThreads[i] {
			t.Fatalf("item %d thread = %q, want %q", i, item.ThreadID, wantThreads[i])
		}
		if !rootPattern.MatchString(item.RootID) {
			t.Fatalf("item %d root_id not opaque: %q", i, item.RootID)
		}
		if item.Kind != "ordinary" || item.Archived {
			t.Fatalf("item %d kind=%q archived=%v, want ordinary non-archived", i, item.Kind, item.Archived)
		}
		if item.LastActivity == "" {
			t.Fatalf("item %d missing last_activity", i)
		}
	}
	if page.Items[0].LastActivity != "2026-10-02T12:05:00Z" {
		t.Fatalf("items[0].last_activity = %q", page.Items[0].LastActivity)
	}
	if page.Items[2].Title != "Fix the login redirect" {
		t.Fatalf("items[2].title = %q", page.Items[2].Title)
	}
	// The two A1 copies share a thread id but live under different roots:
	// identity must be (root_id, thread_id) pairs, not thread ids alone.
	if page.Items[1].RootID == page.Items[2].RootID {
		t.Fatal("multi-root collision: mirrored thread ids need distinct root_ids")
	}
}

func TestConversationsAPIFilters(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	homeA, homeB := conversationsFixtureHomes(t)
	configureConversationsRoots(t, dataDir, homeA, homeB)

	page := conversationsAPIList(t, descriptor, "?filter=all")
	if len(page.Items) != 6 {
		t.Fatalf("filter=all items = %+v, want 6", page.Items)
	}

	page = conversationsAPIList(t, descriptor, "?filter=subagent")
	if len(page.Items) != 2 {
		t.Fatalf("filter=subagent items = %+v, want 2", page.Items)
	}
	for _, item := range page.Items {
		if item.Kind != "subagent" || item.Archived {
			t.Fatalf("subagent filter returned %+v", item)
		}
	}

	page = conversationsAPIList(t, descriptor, "?filter=archived")
	if len(page.Items) != 1 || !page.Items[0].Archived {
		t.Fatalf("filter=archived items = %+v, want one archived", page.Items)
	}

	// "normal" is the accepted alias for the default ordinary view.
	page = conversationsAPIList(t, descriptor, "?filter=normal")
	if len(page.Items) != 3 {
		t.Fatalf("filter=normal items = %+v, want 3", page.Items)
	}
}

func TestConversationsAPIRejectsBadParams(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	homeA, homeB := conversationsFixtureHomes(t)
	configureConversationsRoots(t, dataDir, homeA, homeB)
	homeAAbs, _ := filepath.Abs(homeA)

	for _, tc := range []struct {
		name  string
		query string
		code  string
	}{
		{"unknown filter", "?filter=bogus", "invalid_filter"},
		{"unknown root", "?root=rdeadbeefdeadbeef", "invalid_root"},
		{"path-shaped root", "?root=..%2f..%2fetc", "invalid_root"},
		{"garbage cursor", "?cursor=not-a-cursor", "invalid_cursor"},
		{"non-integer limit", "?limit=abc", "invalid_limit"},
		{"zero limit", "?limit=0", "invalid_limit"},
		{"negative limit", "?limit=-5", "invalid_limit"},
	} {
		status, header, body := conversationsAPIRequest(t, descriptor, "/conversations"+tc.query, descriptor.Token)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", tc.name, status, body)
		}
		if got := header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control = %q, want no-store", tc.name, got)
		}
		var parsed struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error != tc.code {
			t.Fatalf("%s: body = %s, want error=%q", tc.name, body, tc.code)
		}
		if strings.Contains(string(body), homeAAbs) {
			t.Fatalf("%s: error body leaks filesystem path: %s", tc.name, body)
		}
	}
}

func TestConversationsAPIPaginatesWithOpaqueCursor(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	homeA, homeB := conversationsFixtureHomes(t)
	configureConversationsRoots(t, dataDir, homeA, homeB)

	first := conversationsAPIList(t, descriptor, "?filter=all&limit=2")
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, want 2 items + cursor", first)
	}
	if strings.Contains(first.NextCursor, "/") || strings.Contains(first.NextCursor, `\`) {
		t.Fatalf("cursor leaks path separators: %q", first.NextCursor)
	}

	var seen []string
	cursor := first.NextCursor
	seen = append(seen, first.Items[0].ThreadID+"/"+first.Items[0].RootID,
		first.Items[1].ThreadID+"/"+first.Items[1].RootID)
	for cursor != "" {
		page := conversationsAPIList(t, descriptor, "?filter=all&limit=2&cursor="+cursor)
		for _, item := range page.Items {
			seen = append(seen, item.ThreadID+"/"+item.RootID)
		}
		cursor = page.NextCursor
	}
	if len(seen) != 6 {
		t.Fatalf("paginated items = %v, want 6", seen)
	}
	unique := make(map[string]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("duplicate across pages: %v", seen)
		}
		unique[id] = true
	}

	// A cursor is bound to the filter that issued it; replaying it against a
	// different view is a client error, never a wrong page.
	status, _, body := conversationsAPIRequest(t, descriptor,
		"/conversations?filter=ordinary&cursor="+first.NextCursor, descriptor.Token)
	if status != http.StatusBadRequest {
		t.Fatalf("cross-filter cursor status = %d body=%s", status, body)
	}

	// Limit is clamped, never an error: asking for more than the max returns
	// a normal page.
	page := conversationsAPIList(t, descriptor, "?filter=all&limit=9999")
	if len(page.Items) != 6 {
		t.Fatalf("clamped-limit items = %d, want 6", len(page.Items))
	}
}

// The root query parameter scopes the list to one monitored root and the
// response always carries the monitored-root refs (id + compact label) so a
// client can render a stable directory filter.
func TestConversationsAPIRootFilterAndRoots(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	homeA, homeB := conversationsFixtureHomes(t)
	configureConversationsRoots(t, dataDir, homeA, homeB)

	config, err := loadOrCreateConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rootAID, rootBID string
	for _, root := range discoverSessionRoots(config) {
		switch filepath.Base(root.CodexHome) {
		case "homeA":
			rootAID = conversationRootID(root)
		case "homeB":
			rootBID = conversationRootID(root)
		}
	}
	if rootAID == "" || rootBID == "" {
		t.Fatalf("fixture roots not discovered: %q %q", rootAID, rootBID)
	}

	page := conversationsAPIList(t, descriptor, "?filter=all&root="+rootBID)
	if len(page.Items) != 2 {
		t.Fatalf("root-scoped items = %+v, want 2", page.Items)
	}
	for _, item := range page.Items {
		if item.RootID != rootBID {
			t.Fatalf("root filter leaked item from %q", item.RootID)
		}
	}
	// Both monitored roots are reported even though only one matched.
	if len(page.Roots) != 2 {
		t.Fatalf("roots = %+v, want both monitored roots", page.Roots)
	}
	labels := map[string]string{}
	for _, ref := range page.Roots {
		if ref.Label == "" {
			t.Fatalf("root %q carries an empty label", ref.RootID)
		}
		labels[ref.RootID] = ref.Label
	}
	if labels[rootAID] != "homeA" || labels[rootBID] != "homeB" {
		t.Fatalf("root labels = %+v, want home basenames", page.Roots)
	}

	// A cursor minted under one root cannot be replayed under another.
	scoped := conversationsAPIList(t, descriptor, "?filter=all&root="+rootBID+"&limit=1")
	if scoped.NextCursor == "" {
		t.Fatal("expected a scoped next cursor")
	}
	status, _, body := conversationsAPIRequest(t, descriptor,
		"/conversations?filter=all&root="+rootAID+"&cursor="+scoped.NextCursor, descriptor.Token)
	if status != http.StatusBadRequest {
		t.Fatalf("cross-root cursor status = %d body=%s", status, body)
	}
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error != "invalid_cursor" {
		t.Fatalf("cross-root cursor body = %s, want invalid_cursor", body)
	}
	// And replaying the scoped cursor without the root is rejected too.
	status, _, body = conversationsAPIRequest(t, descriptor,
		"/conversations?filter=all&cursor="+scoped.NextCursor, descriptor.Token)
	if status != http.StatusBadRequest {
		t.Fatalf("descoped cursor status = %d body=%s", status, body)
	}
	// Same-root replay continues fine.
	next := conversationsAPIList(t, descriptor, "?filter=all&root="+rootBID+"&cursor="+scoped.NextCursor)
	if len(next.Items) != 1 {
		t.Fatalf("continuation items = %+v, want the remaining 1", next.Items)
	}
}

func TestConversationsAPIEmptyWhenNoRoots(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	configureConversationsRoots(t, dataDir)

	page := conversationsAPIList(t, descriptor, "")
	if page.Items == nil {
		t.Fatal("items must serialize as [], not null")
	}
	if len(page.Items) != 0 || page.NextCursor != "" || page.State != "complete_persisted" {
		t.Fatalf("empty listing = %+v", page)
	}
}

// C-07 acceptance criterion: conversation reads are file-reader only. List
// and item requests must leave the recovery scanner's persisted cursors
// (state.json) byte-identical and must never drop command files.
func TestConversationsAPIReadsDoNotMutateScannerStateOrCommands(t *testing.T) {
	descriptor, dataDir := startTestAPI(t)
	home, rolloutPath := conversationItemsFixtureHome(t, itemsAPIThreadID)
	configureConversationsRoots(t, dataDir, home)

	config, err := loadOrCreateConfig(filepath.Join(dataDir, "config.json"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	roots := discoverSessionRoots(config)
	if len(roots) != 1 {
		t.Fatalf("fixture config discovered %d roots, want 1", len(roots))
	}
	rootID := conversationRootID(roots[0])

	// Seed state.json exactly as the recovery scanner persists it: a file
	// cursor for the very rollout the conversation endpoints are about to read.
	state := newRuntimeState()
	state.Initialized = true
	state.Files[strings.ToLower(filepath.Clean(rolloutPath))] = FileCursor{
		Offset:   7,
		LastSeen: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
	}
	statePath := filepath.Join(dataDir, "state.json")
	if err := writeJSONAtomic(statePath, state); err != nil {
		t.Fatalf("state: %v", err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}

	page := conversationsAPIList(t, descriptor, "?filter=all")
	if len(page.Items) != 1 {
		t.Fatalf("fixture list = %+v", page.Items)
	}
	status, _, body := conversationsAPIRequest(t, descriptor,
		fmt.Sprintf("/conversations/%s/%s/items?limit=5", rootID, itemsAPIThreadID), descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("items status = %d body=%s", status, body)
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conversation reads rewrote state.json (scanner cursors moved)")
	}
	commands, invalid, err := loadControlCommandFiles(filepath.Join(dataDir, "commands"))
	if err != nil || len(commands) != 0 || len(invalid) != 0 {
		t.Fatalf("conversation reads dropped command files: %+v invalid=%v err=%v", commands, invalid, err)
	}
}
