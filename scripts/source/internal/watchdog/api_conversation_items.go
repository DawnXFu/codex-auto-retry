package watchdog

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Conversation items endpoint: GET /conversations/{rootId}/{threadId}/items.
//
// Clients submit only the opaque (root_id, thread_id) identity and opaque
// cursors, never filesystem paths. The catalog whitelist is the authoritative
// gate that maps identity back to a validated rollout file; the handler adds
// a cheap structural rejection before resolve so path-shaped input is refused
// even when it would never match a whitelisted entry.
//
// Concurrency is bounded by a small semaphore: at most
// conversationItemsMaxConcurrent detail reads run at once. Four is enough for
// a single desktop client paging history while following appends, and each
// read is already bounded internally by the reader's byte and time budgets,
// so queued requests wait on their own context rather than a queue limit.
const conversationItemsMaxConcurrent = 4

var conversationItemsSemaphore = make(chan struct{}, conversationItemsMaxConcurrent)

// conversationItemsHandler serves one page of projected conversation items.
// Registered as "GET /conversations/{rootId}/{threadId}/items".
func conversationItemsHandler(service *managementService, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		if !conversationItemsEscapedPathClean(r.URL.EscapedPath()) {
			writeAPIJSON(w, http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
			return
		}
		rootID := r.PathValue("rootId")
		threadID := r.PathValue("threadId")
		if !conversationItemsParamSafe(rootID) || !conversationItemsParamSafe(threadID) {
			writeAPIJSON(w, http.StatusNotFound, map[string]string{"error": "conversation_not_found"})
			return
		}
		limit, err := conversationItemsLimit(r.URL.Query().Get("limit"))
		if err != nil {
			writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_limit"})
			return
		}

		select {
		case conversationItemsSemaphore <- struct{}{}:
			defer func() { <-conversationItemsSemaphore }()
		case <-r.Context().Done():
			writeAPIJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "conversation_read_unavailable"})
			return
		}

		catalog, err := service.conversationCatalog()
		if err != nil {
			writeAPIJSON(w, http.StatusInternalServerError, map[string]string{"error": "conversations_unavailable"})
			return
		}
		reader := newConversationReader(catalog)
		page, err := reader.readItems(r.Context(), rootID, threadID,
			r.URL.Query().Get("cursor"), r.URL.Query().Get("direction"), limit)
		if err != nil {
			status, reason := conversationItemsErrorStatus(err)
			writeAPIJSON(w, status, map[string]string{"error": reason})
			return
		}
		writeAPIJSON(w, http.StatusOK, page)
	}
}

// conversationItemsParamSafe is the structural gate on identity parameters:
// they are opaque identifiers, so any value that looks like a path fragment
// is rejected before the catalog whitelist is consulted.
func conversationItemsParamSafe(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	return !strings.ContainsAny(value, `/\`) && !strings.Contains(value, "..")
}

// conversationItemsEscapedPathClean inspects the still-encoded request path so
// percent-smuggled separators and dots (%2e, %5c, %2f, any case) are refused
// even though PathValue is already decoded by the time the handler sees it.
func conversationItemsEscapedPathClean(escaped string) bool {
	lowered := strings.ToLower(escaped)
	for _, encoded := range []string{"%2e", "%5c", "%2f"} {
		if strings.Contains(lowered, encoded) {
			return false
		}
	}
	return true
}

var errConversationItemsLimit = errors.New("invalid conversation items limit")

// conversationItemsLimit validates the optional page-size parameter, sharing
// the list endpoint's convention: empty selects the reader's default,
// over-large values are clamped there, and zero, negative, or non-numeric
// input is a client error.
func conversationItemsLimit(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 1 {
		return 0, errConversationItemsLimit
	}
	return value, nil
}

// conversationItemsErrorStatus maps reader failures to privacy-safe HTTP
// status and reason strings. Internal detail and conversation content never
// reach the response body.
func conversationItemsErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, errConversationNotFound):
		return http.StatusNotFound, "conversation_not_found"
	case errors.Is(err, errConversationFileChanged):
		return http.StatusConflict, "conversation_changed"
	case errors.Is(err, errConversationCursorInvalid):
		return http.StatusBadRequest, "invalid_cursor"
	case errors.Is(err, errConversationDirectionUnknown):
		return http.StatusBadRequest, "invalid_direction"
	default:
		return http.StatusInternalServerError, "conversations_unavailable"
	}
}
