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
// Roots lists every monitored session root (id + compact label) so a client
// can render a stable root filter even for roots with no matching items.
// Dirs lists every distinct working directory recorded in the filtered scan
// scope (dir value + compact label) so a client's project-directory filter
// stays complete while a dir is selected and while paging.
type conversationsListResponse struct {
	Items      []conversationsListItem `json:"items"`
	Roots      []conversationRootRef   `json:"roots"`
	Dirs       []conversationDirRef    `json:"dirs"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Partial    bool                    `json:"partial"`
	State      string                  `json:"state"`
}

// conversationsListItem is the wire form of one catalog summary. Identity is
// the opaque (root_id, thread_id) pair; no catalog-resolved filesystem path
// ever leaves the process. Title is session metadata the catalog already
// derives inside its bounded head scan, not message content; Cwd is the
// working directory recorded in the session_meta payload (absent when the
// record carries none).
type conversationsListItem struct {
	RootID       string `json:"root_id"`
	ThreadID     string `json:"thread_id"`
	Kind         string `json:"kind"`
	Archived     bool   `json:"archived"`
	Title        string `json:"title,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	LastActivity string `json:"last_activity,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
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

		page, err := service.conversationsList(r.Context(), filter, query.Get("root"), query.Get("dir"), query.Get("cursor"), limit)
		if err != nil {
			if r.Context().Err() != nil {
				return // client is gone; nothing to write to
			}
			switch {
			case errors.Is(err, errConversationFilterUnknown):
				writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_filter"})
			case errors.Is(err, errConversationRootUnknown):
				writeAPIJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_root"})
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

// conversationCatalog builds a whitelist catalog over the session roots the
// config currently monitors. Both conversations endpoints share this
// construction so the whitelist always reflects config.json; rebuilding per
// call is deliberate — the scan itself is bounded by the catalog's file/time
// budgets.
func (m *managementService) conversationCatalog() (*conversationCatalog, error) {
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return nil, err
	}
	return newConversationCatalog(discoverSessionRoots(config)), nil
}

// conversationsList returns one bounded page from the shared catalog.
func (m *managementService) conversationsList(ctx context.Context, filter conversationFilter, rootID, dir, cursor string, limit int) (conversationListPage, error) {
	catalog, err := m.conversationCatalog()
	if err != nil {
		return conversationListPage{}, err
	}
	return catalog.list(ctx, filter, rootID, dir, cursor, limit)
}

func conversationsListResponseFor(page conversationListPage) conversationsListResponse {
	response := conversationsListResponse{
		Items:      make([]conversationsListItem, 0, len(page.Items)),
		Roots:      page.Roots,
		Dirs:       page.Dirs,
		NextCursor: page.NextCursor,
		Partial:    page.Partial,
		State:      conversationStateCompletePersisted,
	}
	if response.Roots == nil {
		response.Roots = []conversationRootRef{}
	}
	if response.Dirs == nil {
		response.Dirs = []conversationDirRef{}
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
			Cwd:          summary.Cwd,
		})
	}
	return response
}
