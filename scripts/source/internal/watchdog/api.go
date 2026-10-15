package watchdog

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiDescriptor is api.json: the discovery document desktop clients read to
// find the per-launch loopback endpoint and bearer token. Port and token
// rotate every launch, so the file is deliberately left behind on shutdown —
// a stale descriptor fails pid verification on the client side.
type apiDescriptor struct {
	Version   int    `json:"version"`
	Port      int    `json:"port"`
	Token     string `json:"token"`
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

const (
	apiFileName          = "api.json"
	apiDescriptorVersion = 1
	apiTokenBytes        = 16
)

// serveManagementAPI runs the loopback management surface until ctx ends. It
// is a best-effort sidecar: any startup failure only disables the API, the
// watchdog loop is never delayed by it.
func serveManagementAPI(ctx context.Context, service *managementService, dataDir string, logger *safeLogger) {
	token, err := newAPIToken()
	if err != nil {
		logger.Printf("management api disabled category=api_token")
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Printf("management api disabled category=api_bind")
		return
	}
	port := listener.Addr().(*net.TCPAddr).Port
	descriptor := apiDescriptor{
		Version:   apiDescriptorVersion,
		Port:      port,
		Token:     token,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeJSONAtomic(filepath.Join(dataDir, apiFileName), descriptor); err != nil {
		_ = listener.Close()
		logger.Printf("management api disabled category=api_descriptor")
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !bearerAuthorized(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeAPIJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		snapshot, err := service.snapshot(time.Now().UTC())
		if err != nil {
			writeAPIJSON(w, http.StatusInternalServerError, map[string]string{"error": "snapshot_unavailable"})
			return
		}
		writeAPIJSON(w, http.StatusOK, snapshot)
	})
	mux.HandleFunc("GET /conversations", conversationsListHandler(service, token))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Printf("management api listening port=%d", port)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("management api stopped category=api_error")
	}
}

func newAPIToken() (string, error) {
	var raw [apiTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func bearerAuthorized(r *http.Request, token string) bool {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(token)) == 1
}

func writeAPIJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
