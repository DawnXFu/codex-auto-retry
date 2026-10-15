package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// startTestAPI boots the real server on a temp data dir and returns the
// parsed descriptor plus the dir; cleanup stops the server.
func startTestAPI(t *testing.T) (apiDescriptor, string) {
	t.Helper()
	dataDir := t.TempDir()
	logger, err := newSafeLogger(filepath.Join(dataDir, "logs", "daemon.log"))
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go serveManagementAPI(ctx, newManagementService(dataDir), dataDir, logger)

	var descriptor apiDescriptor
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(dataDir, apiFileName))
		if err != nil {
			return false
		}
		return json.Unmarshal(data, &descriptor) == nil && descriptor.Port > 0 && descriptor.Token != ""
	})
	t.Cleanup(func() {
		cancel()
		logger.Close()
	})
	if descriptor.Version != apiDescriptorVersion || descriptor.PID != os.Getpid() || descriptor.StartedAt == "" {
		t.Fatalf("descriptor malformed: %+v", descriptor)
	}
	return descriptor, dataDir
}

func apiRequest(t *testing.T, descriptor apiDescriptor, path, token string) (int, []byte) {
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
	return response.StatusCode, body
}

func TestManagementAPIAuthAndSnapshot(t *testing.T) {
	descriptor, _ := startTestAPI(t)

	// Every management API response carries Cache-Control: no-store —
	// snapshot and liveness payloads are private state, never cacheable.
	status, header, _ := conversationsAPIRequest(t, descriptor, "/healthz", "")
	if status != http.StatusOK {
		t.Fatalf("healthz status = %d", status)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("healthz Cache-Control = %q, want no-store", got)
	}
	status, header, _ = conversationsAPIRequest(t, descriptor, "/snapshot", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("snapshot without token status = %d", status)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("snapshot 401 Cache-Control = %q, want no-store", got)
	}
	if status, _ = apiRequest(t, descriptor, "/snapshot", "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("snapshot with wrong token status = %d", status)
	}
	status, header, body := conversationsAPIRequest(t, descriptor, "/snapshot", descriptor.Token)
	if status != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s", status, body)
	}
	if got := header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("snapshot Cache-Control = %q, want no-store", got)
	}
	var snapshot ManagementSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatalf("snapshot body is not a ManagementSnapshot: %v", err)
	}
	if snapshot.Version == "" {
		t.Fatal("snapshot missing version")
	}
}

func TestManagementAPIDescriptorPrivateAndRotating(t *testing.T) {
	first, firstDir := startTestAPI(t)
	second, _ := startTestAPI(t)
	if first.Token == second.Token || first.Port == second.Port {
		t.Fatal("token and port must rotate per launch")
	}
	info, err := os.Stat(filepath.Join(firstDir, apiFileName))
	if err != nil {
		t.Fatalf("api.json missing: %v", err)
	}
	// writeJSONAtomic chmods the temp file 0600 before rename; on Windows the
	// user-private ACL comes from the data directory itself.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("api.json mode = %o, want 0600", info.Mode().Perm())
	}
}
