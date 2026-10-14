package watchdog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"unicode/utf8"
)

// Titles come from the head of a rollout file: an explicit title on
// session_meta when Codex writes one, otherwise the first real user prompt.
// The scan is bounded so snapshot assembly stays cheap even over slow disks.
const (
	maxTitleScanLines = 64
	maxTitleScanBytes = 512 * 1024
	maxTitleRunes     = 80
)

// Injected context wrappers are authored by Codex, not the user. Using one as
// a title would leak environment details and look like noise in the client.
var titleWrapperPrefixes = []string{
	"<environment_context",
	"<permissions",
	"<user_instructions",
	"<collaboration_mode",
	"<app_context",
}

func readThreadTitle(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	scanned := 0
	for line := 0; line < maxTitleScanLines && scanned < maxTitleScanBytes; line++ {
		raw, readErr := reader.ReadBytes('\n')
		scanned += len(raw)
		if title := titleFromRolloutLine(raw); title != "" {
			return title
		}
		if readErr != nil {
			break
		}
	}
	return ""
}

func titleFromRolloutLine(line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return ""
	}
	var envelope eventEnvelope
	if json.Unmarshal(line, &envelope) != nil {
		return ""
	}
	switch envelope.Type {
	case "session_meta":
		var payload struct {
			Title      string `json:"title"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(envelope.Payload, &payload) != nil {
			return ""
		}
		if title := normalizeTitle(payload.Title); title != "" {
			return title
		}
		return normalizeTitle(payload.ThreadName)
	case "event_msg":
		var payload struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Payload, &payload) != nil || payload.Type != "user_message" {
			return ""
		}
		return normalizeTitle(payload.Message)
	case "response_item":
		var payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(envelope.Payload, &payload) != nil ||
			payload.Type != "message" || payload.Role != "user" {
			return ""
		}
		for _, item := range payload.Content {
			if item.Type != "input_text" && item.Type != "text" {
				continue
			}
			if title := normalizeTitle(item.Text); title != "" {
				return title
			}
		}
	}
	return ""
}

func normalizeTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	for _, prefix := range titleWrapperPrefixes {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	if utf8.RuneCountInString(text) > maxTitleRunes {
		text = string([]rune(text)[:maxTitleRunes])
	}
	return text
}
