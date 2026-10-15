package watchdog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const itemsAPIThreadID = "019fc0c1-00a1-7ca1-80a1-0000000000a1"

// conversationItemsFixtureHome builds a hermetic Codex home with one rollout
// and returns it plus the file path. The API discovers it through
// session_roots in config.json, never through a real Codex home.
func conversationItemsFixtureHome(t *testing.T, threadID string) (string, string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeRollout(t, dir, "rollout-2026-10-03T10-00-00-"+threadID+".jsonl", []string{
		readerMetaLine(threadID, "2026-10-03T10:00:00.000Z"),
		`{"timestamp":"2026-10-03T10:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"hello from api"}}`,
		`{"timestamp":"2026-10-03T10:02:00.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer from api"}]}}`,
	})
	return home, path
}

// conversationItemsTestAPI boots the real server with a config that
// whitelists only the fixture home, then returns the descriptor and the
// opaque root id the API will accept for that home.
func conversationItemsTestAPI(t *testing.T, home string) (apiDescriptor, string) {
	t.Helper()
	descriptor, dataDir := startTestAPI(t)
	cfg := isolatedConfig(home)
	if err := writeJSONAtomic(filepath.Join(dataDir, "config.json"), cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	roots := discoverSessionRoots(cfg)
	if len(roots) != 1 {
		t.Fatalf("fixture config discovered %d roots, want 1", len(roots))
	}
	return descriptor, conversationRootID(roots[0])
}

// conversationItemsRequest is apiRequest plus response headers: this
// endpoint must answer with Cache-Control: no-store on every response.
func conversationItemsRequest(t *testing.T, descriptor apiDescriptor, path, token string) (int, http.Header, []byte) {
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

func TestConversationItemsAPIAuthPageAndFollow(t *testing.T) {
	home, path := conversationItemsFixtureHome(t, itemsAPIThreadID)
	descriptor, rootID := conversationItemsTestAPI(t, home)
	base := fmt.Sprintf("/conversations/%s/%s/items", rootID, itemsAPIThreadID)

	if status, _, _ := conversationItemsRequest(t, descriptor, base, ""); status != http.StatusUnauthorized {
		t.Fatalf("items without token status = %d", status)
	}
	if status, _, _ := conversationItemsRequest(t, descriptor, base, "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("items with wrong token status = %d", status)
	}

	status, header, body := conversationItemsRequest(t, descriptor, base+"?limit=2", descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("items status = %d body=%s", status, body)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var page conversationItemPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("page body does not decode: %v", err)
	}
	if page.State != conversationStateCompletePersisted {
		t.Fatalf("state = %q", page.State)
	}
	if len(page.Items) != 2 || page.Items[0].Kind != conversationItemUserMessage ||
		page.Items[1].Kind != conversationItemAssistantMessage {
		t.Fatalf("first page wrong: %+v", page.Items)
	}
	if page.NextCursor == "" || page.TailCursor == "" {
		t.Fatalf("cursors missing: %+v", page)
	}
	if page.Kind != "ordinary" || page.Archived {
		t.Fatalf("page flags wrong: %+v", page)
	}

	// The next page carries the remaining item and no further cursor.
	status, _, body = conversationItemsRequest(t, descriptor,
		base+"?limit=2&cursor="+page.NextCursor, descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("continuation status = %d body=%s", status, body)
	}
	var older conversationItemPage
	if err := json.Unmarshal(body, &older); err != nil {
		t.Fatalf("continuation body does not decode: %v", err)
	}
	if len(older.Items) != 1 || older.NextCursor != "" {
		t.Fatalf("continuation page wrong: %+v", older)
	}
	combined := append(older.Items, page.Items...)
	texts := []string{combined[0].Text, combined[1].Text, combined[2].Text}
	if texts[1] != "hello from api" || texts[2] != "answer from api" {
		t.Fatalf("paged union lost content: %+v", combined)
	}

	// A forward poll from the tail anchor picks up an appended record alone.
	appendRollout(t, path,
		`{"timestamp":"2026-10-03T10:03:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"appended"}}`+"\n")
	status, _, body = conversationItemsRequest(t, descriptor,
		base+"?direction=forward&cursor="+page.TailCursor, descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("forward status = %d body=%s", status, body)
	}
	var follow conversationItemPage
	if err := json.Unmarshal(body, &follow); err != nil {
		t.Fatalf("forward body does not decode: %v", err)
	}
	if len(follow.Items) != 1 || follow.Items[0].Text != "appended" {
		t.Fatalf("forward follow wrong: %+v", follow.Items)
	}
}

func TestConversationItemsAPIRejectsIdentitySmuggling(t *testing.T) {
	home, _ := conversationItemsFixtureHome(t, itemsAPIThreadID)
	descriptor, rootID := conversationItemsTestAPI(t, home)

	cases := map[string]string{
		"dotdot":              fmt.Sprintf("/conversations/%s/../items", rootID),
		"encoded dot":         fmt.Sprintf("/conversations/%s/%%2e%%2e/items", rootID),
		"backslash":           fmt.Sprintf("/conversations/%s/a%%5cb/items", rootID),
		"encoded separator":   fmt.Sprintf("/conversations/%s/a%%2fb/items", rootID),
		"absolute windows":    fmt.Sprintf("/conversations/%s/c%%3a%%5ctmp%%5cx.jsonl/items", rootID),
		"unknown root":        "/conversations/rdeadbeefdeadbeef/" + itemsAPIThreadID + "/items",
		"absent thread":       fmt.Sprintf("/conversations/%s/%s/items", rootID, catalogThreadB1),
		"non-uuid thread":     fmt.Sprintf("/conversations/%s/not-a-thread/items", rootID),
		"root with traversal": fmt.Sprintf("/conversations/..%%2fhome/%s/items", itemsAPIThreadID),
	}
	for name, path := range cases {
		status, _, body := conversationItemsRequest(t, descriptor, path, descriptor.Token)
		if status != http.StatusNotFound {
			t.Fatalf("%s: status = %d body=%s, want 404", name, status, body)
		}
	}
	// A valid page on the same server proves the rejections were about the
	// identity parameters, not a dead endpoint.
	status, _, body := conversationItemsRequest(t, descriptor,
		fmt.Sprintf("/conversations/%s/%s/items", rootID, itemsAPIThreadID), descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("control request status = %d body=%s", status, body)
	}
}

func TestConversationItemsAPIErrorMapping(t *testing.T) {
	home, path := conversationItemsFixtureHome(t, itemsAPIThreadID)
	descriptor, rootID := conversationItemsTestAPI(t, home)
	base := fmt.Sprintf("/conversations/%s/%s/items", rootID, itemsAPIThreadID)

	for name, query := range map[string]string{
		"bad direction":  "direction=sideways",
		"bad cursor":     "cursor=not-a-cursor",
		"bad limit":      "limit=abc",
		"negative limit": "limit=-3",
	} {
		status, _, body := conversationItemsRequest(t, descriptor, base+"?"+query, descriptor.Token)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", name, status, body)
		}
		if strings.Contains(string(body), home) || strings.Contains(string(body), path) {
			t.Fatalf("%s: error body leaked filesystem detail: %s", name, body)
		}
	}

	// A cursor minted before truncation is invalidated by the file change.
	status, _, body := conversationItemsRequest(t, descriptor, base+"?limit=1", descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("seed page status = %d body=%s", status, body)
	}
	var seeded conversationItemPage
	if err := json.Unmarshal(body, &seeded); err != nil || seeded.NextCursor == "" {
		t.Fatalf("seed page decode/cursor: %v %+v", err, seeded)
	}
	if err := os.WriteFile(path, []byte(readerMetaLine(itemsAPIThreadID, "2026-10-03T10:00:00.000Z")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, _, body = conversationItemsRequest(t, descriptor,
		base+"?cursor="+seeded.NextCursor, descriptor.Token)
	if status != http.StatusConflict {
		t.Fatalf("stale cursor status = %d body=%s, want 409", status, body)
	}
	if strings.Contains(string(body), home) || strings.Contains(string(body), path) {
		t.Fatalf("conflict body leaked filesystem detail: %s", body)
	}
}
