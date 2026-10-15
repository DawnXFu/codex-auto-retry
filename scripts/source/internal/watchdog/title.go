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
	"<external_codex_apps",
}

// leadingWrapperTag reports whether text opens with a "<name...>" tool or
// wrapper tag rather than real user text. Wrapper names are snake_case
// identifiers — they always contain an underscore (environment_context,
// external_codex_apps_open_page, ...) — so a leading "<3", "<-", or "<div>"
// stays eligible as a title.
func leadingWrapperTag(text string) bool {
	if !strings.HasPrefix(text, "<") {
		return false
	}
	name := text[1:]
	if end := strings.IndexAny(name, " >"); end >= 0 {
		name = name[:end]
	}
	underscore := false
	for i, c := range name {
		switch {
		case c == '_':
			underscore = true
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return underscore && len(name) > 0
}

func readThreadTitle(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	scanned := 0
	derived, explicit := "", ""
	for line := 0; line < maxTitleScanLines && scanned < maxTitleScanBytes; line++ {
		raw, readErr := reader.ReadBytes('\n')
		scanned += len(raw)
		if title := explicitTitleFromRolloutLine(raw); title != "" {
			explicit = title
		}
		if derived == "" {
			derived = derivedTitleFromRolloutLine(raw)
		}
		if readErr != nil {
			break
		}
	}
	// An explicit persisted signal (session_meta title, a rename record)
	// outranks anything derived from message text.
	if explicit != "" {
		return explicit
	}
	return derived
}

// explicitTitleFromRolloutLine extracts a title the rollout asserts as
// metadata: a session_meta title/thread_name, or a thread_name_updated
// rename record (the same record the conversation reader recognizes as the
// thread_renamed boundary). A rename is the user's own naming act, so the
// latest one wins over earlier explicit signals and any derived text.
func explicitTitleFromRolloutLine(line []byte) string {
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
			Type  string `json:"type"`
			Name  string `json:"name"`
			Title string `json:"title"`
		}
		if json.Unmarshal(envelope.Payload, &payload) != nil || payload.Type != "thread_name_updated" {
			return ""
		}
		if title := normalizeTitle(payload.Name); title != "" {
			return title
		}
		return normalizeTitle(payload.Title)
	}
	return ""
}

// derivedTitleFromRolloutLine extracts a title from user-authored message
// text — event_msg user_message or a response_item user message — with
// Codex-injected boilerplate stripped by normalizeTitle.
func derivedTitleFromRolloutLine(line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return ""
	}
	var envelope eventEnvelope
	if json.Unmarshal(line, &envelope) != nil {
		return ""
	}
	switch envelope.Type {
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

// agentsInstructionsMarker opens the AGENTS.md block Codex prepends to the
// first persisted user message. The header plus its <INSTRUCTIONS> envelope
// is injected boilerplate, not user text.
const agentsInstructionsMarker = "# AGENTS.md instructions"

// injectedBlockNames are the tags Codex wraps injected context in inside a
// persisted user message. INSTRUCTIONS carries the AGENTS.md envelope; the
// rest mirror titleWrapperPrefixes for mid-text occurrence.
var injectedBlockNames = []string{
	"environment_context",
	"permissions",
	"user_instructions",
	"collaboration_mode",
	"app_context",
	"external_codex_apps",
	"INSTRUCTIONS",
}

// stripInjectedBlocks removes Codex-authored boilerplate embedded inside a
// user message: the AGENTS.md header plus its <INSTRUCTIONS>…</INSTRUCTIONS>
// envelope and every known context block, wherever they sit in the text.
// The text the user actually typed survives; a message that is only
// injection reduces to "".
func stripInjectedBlocks(text string) string {
	for {
		idx := strings.Index(text, agentsInstructionsMarker)
		if idx < 0 {
			break
		}
		rest := text[idx:]
		if end := strings.Index(rest, "</INSTRUCTIONS>"); end >= 0 {
			text = text[:idx] + rest[end+len("</INSTRUCTIONS>"):]
			continue
		}
		// A header without its envelope: drop just the header line so typed
		// text further down still counts.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			text = text[:idx] + rest[nl+1:]
		} else {
			text = text[:idx]
		}
	}
	for _, name := range injectedBlockNames {
		text = cutTaggedBlocks(text, name)
	}
	return text
}

// cutTaggedBlocks removes every <name…>…</name> block from text. A tag name
// matches only as a whole word (<environment_context…> never collides with
// <environment_contextual>). An unclosed envelope swallows to the end of the
// text: injected blocks are machine-authored and a dangling one only appears
// when the record was cut mid-write.
func cutTaggedBlocks(text, name string) string {
	open := "<" + name
	close := "</" + name + ">"
	for {
		start := indexTagOpen(text, open)
		if start < 0 {
			return text
		}
		rest := text[start+len(open):]
		gt := strings.IndexByte(rest, '>')
		if gt < 0 {
			return text[:start]
		}
		if strings.HasSuffix(strings.TrimSpace(rest[:gt]), "/") {
			// Self-closing tag: drop the tag itself, keep following text.
			text = text[:start] + rest[gt+1:]
			continue
		}
		if end := strings.Index(rest[gt+1:], close); end >= 0 {
			text = text[:start] + rest[gt+1+end+len(close):]
			continue
		}
		return text[:start]
	}
}

// indexTagOpen finds the next "<name" whose following byte ends the tag name
// (">", "/", or whitespace), so prefixed tags like
// <external_codex_apps_open_page> never match the shorter name.
func indexTagOpen(text, open string) int {
	for from := 0; ; {
		idx := strings.Index(text[from:], open)
		if idx < 0 {
			return -1
		}
		pos := from + idx + len(open)
		if pos >= len(text) {
			return from + idx
		}
		switch text[pos] {
		case '>', '/', ' ', '\t', '\n', '\r':
			return from + idx
		}
		from += idx + 1
	}
}

func normalizeTitle(text string) string {
	text = stripInjectedBlocks(text)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	for _, prefix := range titleWrapperPrefixes {
		if strings.HasPrefix(text, prefix) {
			return ""
		}
	}
	if leadingWrapperTag(text) {
		return ""
	}
	if utf8.RuneCountInString(text) > maxTitleRunes {
		text = string([]rune(text)[:maxTitleRunes])
	}
	return text
}
