package watchdog

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// conversationsScanSlots bounds concurrent catalog scans. Every list request
// walks the monitored session roots, so a burst of callers could otherwise
// stack unbounded filesystem reads on the loopback surface. Four concurrent
// scans is deliberately small: additional requests wait for a slot (each scan
// is internally bounded to maxCatalogScanDuration, ~4s) or until the client
// disconnects.
var conversationsScanSlots = make(chan struct{}, 4)

// conversationsListResponse is the wire shape of GET /conversations. Partial
// and State are always serialized so clients can rely on them; State reports
// whether the catalog scan completed within its bounds
// (complete_persisted | partial), mirroring the detail endpoint's page state.
type conversationsListResponse struct {
	Items      []conversationsListItem `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Partial    bool                    `json:"partial"`
	State      string                  `json:"state"`
}

// conversationsListItem is the wire form of one catalog summary. Identity is
// the opaque (root_id, thread_id) pair; no filesystem path ever leaves the
// process. Title is session metadata the catalog already derives inside its
// bounded head scan, not message content.
type conversationsListItem struct {
	RootID       string `json:"root_id"`
	ThreadID     string `json:"thread_id"`
	Kind         string `json:"kind"`
	Archived     bool   `json:"archived"`
	Title        string `json:"title,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	LastActivity string `json:"last_activity,omitempty"`
}

// conversationsListHandler serves GET /conversations. It is registered on the
// management mux in api.go and follows the same per-handler bearer contract
// as /snapshot; writeAPIJSON attaches Cache-Control: no-store to every
// response, including errors.
func conversationsListHandler(service *managementService, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		query := r.URL.Query()
		filter, err := parseConversationFilter(query.Get("filter"))
		if err != nil {
			writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
			return
		}
		limit, err := conversationsParseLimit(query.Get("limit"))
		if err != nil {
			writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_limit"})
			return
		}

		select {
		case conversationsScanSlots <- struct{}{}:
			defer func() { <-conversationsScanSlots }()
		case <-r.Context().Done():
			return
		}

		page, err := service.conversationsList(r.Context(), filter, query.Get("cursor"), limit)
		if err != nil {
			if r.Context().Err() != nil {
				return // client is gone; nothing to write to
			}
			switch {
			case errors.Is(err, errConversationFilterUnknown):
				writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
			case errors.Is(err, errConversationCursorInvalid):
				writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_cursor"})
			default:
				// Internal failures surface as a generic reason only: no
				// paths, catalog detail, or conversation content in bodies.
				writeAPIJSON(w, http.StatusInternalServerError, map[string]string{"error": "conversations_unavailable"})
			}
			return
		}
		writeAPIJSON(w, http.StatusOK, conversationsListResponseFor(page))
	}
}

// conversationsParseLimit validates the optional limit parameter. Values must
// be positive integers; the catalog clamps the result to its page bound, so
// oversized values are legal here and shrink to the max.
func conversationsParseLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, errors.New("invalid conversations limit")
	}
	return limit, nil
}

// conversationsList builds a per-request catalog over the session roots the
// config currently monitors and returns one bounded page. Rebuilding per call
// keeps the whitelist in step with config.json; the scan itself is bounded by
// the catalog's file/time budgets.
func (m *managementService) conversationsList(ctx context.Context, filter conversationFilter, cursor string, limit int) (conversationListPage, error) {
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return conversationListPage{}, err
	}
	catalog := newConversationCatalog(discoverSessionRoots(config))
	return catalog.list(ctx, filter, cursor, limit)
}

func conversationsListResponseFor(page conversationListPage) conversationsListResponse {
	response := conversationsListResponse{
		Items:      make([]conversationsListItem, 0, len(page.Items)),
		NextCursor: page.NextCursor,
		Partial:    page.Partial,
		State:      conversationStateCompletePersisted,
	}
	if page.Partial {
		response.State = conversationStatePartial
	}
	for _, summary := range page.Items {
		response.Items = append(response.Items, conversationsListItem{
			RootID:       summary.RootID,
			ThreadID:     summary.ThreadID,
			Kind:         summary.Kind,
			Archived:     summary.Archived,
			Title:        summary.Title,
			StartedAt:    summary.StartedAt,
			LastActivity: summary.LastActive,
		})
	}
	return response
}
