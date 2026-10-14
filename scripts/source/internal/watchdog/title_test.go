package watchdog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRollout(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadThreadTitleFromFirstUserMessage(t *testing.T) {
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0423"
	path := writeRollout(t, t.TempDir(), threadID+".jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","cwd":"C:\\repo"}}`,
		`{"timestamp":"2026-10-15T03:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>C:\\repo</cwd>\n</environment_context>"}]}}`,
		`{"timestamp":"2026-10-15T03:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"  Fix the flaky   auth redirect test\nplease  "}]}}`,
	})
	if title := readThreadTitle(path); title != "Fix the flaky auth redirect test please" {
		t.Fatalf("title = %q", title)
	}
}

func TestReadThreadTitlePrefersSessionMeta(t *testing.T) {
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0424"
	path := writeRollout(t, t.TempDir(), threadID+".jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `","title":"Named task"}}`,
		`{"timestamp":"2026-10-15T03:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do things"}]}}`,
	})
	if title := readThreadTitle(path); title != "Named task" {
		t.Fatalf("title = %q", title)
	}
}

func TestReadThreadTitleTruncatesAndSkipsWrappers(t *testing.T) {
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0425"
	long := strings.Repeat("很", maxTitleRunes+20)
	wrapped := writeRollout(t, t.TempDir(), threadID+"_a.jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
	})
	if title := readThreadTitle(wrapped); title != "" {
		t.Fatalf("wrapper-only rollout produced title %q", title)
	}
	truncated := writeRollout(t, t.TempDir(), threadID+"_b.jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + long + `"}]}}`,
	})
	if title := readThreadTitle(truncated); len([]rune(title)) != maxTitleRunes {
		t.Fatalf("title runes = %d", len([]rune(title)))
	}
	if title := readThreadTitle(filepath.Join(t.TempDir(), "missing.jsonl")); title != "" {
		t.Fatalf("missing file produced title %q", title)
	}
}

func TestManagedRetriesResolveTitle(t *testing.T) {
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0426"
	rollout := writeRollout(t, t.TempDir(), threadID+".jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"Refactor the scanner loop"}}`,
	})
	now := time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC)
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{Pending: &PendingRetry{
		Class: classServer, DueAt: now.Add(time.Minute), Attempt: 1, MaxAttempts: 15,
		ConsecutiveRetry: 1, MaxConsecutive: 5, RolloutPath: rollout,
	}}
	rows := managedRetries(state, now, func(id string, candidates []string) string {
		if id != threadID || len(candidates) != 1 || candidates[0] != rollout {
			t.Fatalf("resolver got thread=%s candidates=%v", id, candidates)
		}
		return "Refactor the scanner loop"
	})
	if len(rows) != 1 || rows[0].Title != "Refactor the scanner loop" {
		t.Fatalf("title not threaded through: %+v", rows)
	}
}

func TestSnapshotPopulatesTitleFromRollout(t *testing.T) {
	dataDir := t.TempDir()
	service := newManagementService(dataDir)
	now := time.Now().UTC()
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0427"
	rollout := writeRollout(t, dataDir, threadID+".jsonl", []string{
		`{"timestamp":"2026-10-15T03:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Explain the quota window logic"}]}}`,
	})
	state := newRuntimeState()
	state.Threads[threadID] = ThreadState{QuotaWait: &QuotaWait{
		Class: classRateLimit, DueAt: now.Add(time.Hour), WaitUntil: now.Add(time.Hour),
		RolloutPath: rollout,
	}}
	if err := writeJSONAtomic(service.statePath, state); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(service.statusPath, StatusSnapshot{
		Version: appVersion, Running: true, PID: os.Getpid(), LastScanAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Retries) != 1 || snapshot.Retries[0].Title != "Explain the quota window logic" {
		t.Fatalf("snapshot title missing: %+v", snapshot.Retries)
	}
	// The cached candidate key must keep the second snapshot file-read-free.
	if len(service.titles) != 1 {
		t.Fatalf("title cache = %v", service.titles)
	}
}
