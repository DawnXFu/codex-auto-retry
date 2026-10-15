package watchdog

import (
	"context"
	"errors"
	"strings"
	"time"
)

// accountQuotaReadTimeout bounds one attach-and-read cycle: endpoint
// verification, the WebSocket handshake, initialize, and the
// account/rateLimits/read call together. It is deliberately tighter than the
// appServerRPCClient per-call deadline because this is a display read — a
// backend that cannot answer within it is reported unavailable so the caller
// can fall back to rollout evidence.
const accountQuotaReadTimeout = 5 * time.Second

// Account quota read failure reasons use the same privacy-safe snake_case
// taxonomy as controller state reasons; they classify the failure without
// exposing app-server error bodies.
const (
	// accountQuotaReasonUnsupported — the platform or configured server can
	// never provide a verified endpoint (non-Windows, non-manager server).
	accountQuotaReasonUnsupported = "shared_app_server_unsupported"
	// accountQuotaReasonNotRunning — no usable endpoint is recorded: the
	// shared app-server has not been started, or its state file is missing
	// or unreadable.
	accountQuotaReasonNotRunning = "shared_app_server_not_running"
	// accountQuotaReasonUnverified — an endpoint is recorded but the
	// ownership, Codex-home, or live-process check failed, so attaching to
	// it is not allowed.
	accountQuotaReasonUnverified = "shared_app_server_unverified"
	// accountQuotaReasonUnreachable — the verified endpoint refused the
	// connection before the read could be issued.
	accountQuotaReasonUnreachable = "shared_app_server_unreachable"
	// accountQuotaReasonReadFailed — the endpoint answered, but the
	// account/rateLimits/read call failed or returned undecodable data.
	accountQuotaReasonReadFailed = "account_quota_read_failed"
	// accountQuotaReasonTimeout — the attach-and-read cycle exceeded its
	// display-read budget.
	accountQuotaReasonTimeout = "account_quota_read_timeout"
)

// verifiedSharedEndpoint identifies one attach target after ownership and
// Codex-home verification. Endpoint, PID, and StartedAt together pin a
// backend generation: a restart or reconfiguration writes a new
// shared-server.json record, so comparing values lets the cache layer
// invalidate evidence and late responses from a previous generation.
type verifiedSharedEndpoint struct {
	Endpoint  string
	PID       int
	StartedAt time.Time
	CodexHome string
}

// accountQuotaEndpointSource is the attach-only discovery seam. An
// implementation returns the endpoint of an already-running shared
// app-server only after plugin ownership and Codex home are verified; it
// must never start, adopt, or reconfigure a backend. An error means no
// verified endpoint exists.
type accountQuotaEndpointSource interface {
	VerifiedEndpoint(context.Context) (verifiedSharedEndpoint, error)
}

// accountQuotaGeneration is the typed verdict one read result carries about
// the endpoint generation it ran against. The reader stamps it at result
// construction so the cache layer decides evidence invalidation from the
// field instead of re-deriving it from the diagnostics Reason string.
type accountQuotaGeneration int

const (
	// accountQuotaGenerationInconclusive is the zero value: the result says
	// nothing about the endpoint generation (timeouts and other undecidable
	// failures), so cached evidence must be kept.
	accountQuotaGenerationInconclusive accountQuotaGeneration = iota
	// accountQuotaGenerationVerified means the read ran against a verified
	// endpoint recorded in Backend — including reads that then failed at the
	// dial or call stage. The cache compares Backend against the cached
	// generation to decide.
	accountQuotaGenerationVerified
	// accountQuotaGenerationDisproved means verification proved no usable
	// endpoint generation exists now (not running, ownership unverifiable,
	// or unsupported): cached freshness cannot be claimed.
	accountQuotaGenerationDisproved
)

// accountQuotaReadResult is the typed outcome of one attach-only read. Every
// failure mode lands in Reason with Available false rather than surfacing as
// an error, so the cache layer (single-flight refresh, generation
// invalidation, rollout fallback) can decide freshness without unwrapping
// errors.
type accountQuotaReadResult struct {
	Available bool
	Reason    string
	// Generation records what this result proves about the endpoint
	// generation it ran against; see accountQuotaGeneration.
	Generation accountQuotaGeneration
	// Limits is the decoded account/rateLimits/read response; it is zero
	// unless Available is true.
	Limits appAccountRateLimitsReadResult
	// Backend is the verified endpoint generation that produced Limits. It is
	// also populated on failures that happen after endpoint verification (dial
	// or read), so the cache layer can tell "same generation, transient
	// failure" apart from "the verified endpoint moved to a new generation".
	Backend verifiedSharedEndpoint
	// ObservedAt is when the verified endpoint produced this evidence.
	ObservedAt time.Time
}

// AccountID is the account-generation handle for consistency checks. It is
// never serialized (see appAccountRateLimitsReadResult).
func (r accountQuotaReadResult) AccountID() string {
	return r.Limits.AccountID()
}

// accountQuotaReader is the small interface behind the account quota
// feature: one method hides endpoint verification, the short-lived JSON-RPC
// connection, and the account/rateLimits/read call. Implementations are
// attach-only — they never run an Ensure/start path.
type accountQuotaReader interface {
	ReadAccountQuota(context.Context) accountQuotaReadResult
}

// sharedAccountQuotaReader is the production adapter: it discovers the
// verified shared app-server endpoint, dials it, reads, and closes the
// connection on every call because appServerRPCClient is synchronous and not
// safe for concurrent use.
type sharedAccountQuotaReader struct {
	source  accountQuotaEndpointSource
	timeout time.Duration
}

func newAccountQuotaReader(source accountQuotaEndpointSource) *sharedAccountQuotaReader {
	return &sharedAccountQuotaReader{source: source, timeout: accountQuotaReadTimeout}
}

func (r *sharedAccountQuotaReader) ReadAccountQuota(ctx context.Context) accountQuotaReadResult {
	if r == nil || r.source == nil {
		return unavailableAccountQuota(accountQuotaReasonUnsupported, accountQuotaGenerationDisproved)
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = accountQuotaReadTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint, err := r.source.VerifiedEndpoint(readCtx)
	if err != nil {
		reason, generation := accountQuotaEndpointOutcome(readCtx, err)
		return unavailableAccountQuota(reason, generation)
	}
	if strings.TrimSpace(endpoint.Endpoint) == "" {
		return unavailableAccountQuota(accountQuotaReasonNotRunning, accountQuotaGenerationDisproved)
	}
	client, err := dialAppServerRPC(readCtx, endpoint.Endpoint)
	if err != nil {
		return unavailableAccountQuotaAt(endpoint, accountQuotaCallReason(readCtx, err, accountQuotaReasonUnreachable))
	}
	defer client.Close()
	limits, err := readAppAccountRateLimits(readCtx, client)
	if err != nil {
		return unavailableAccountQuotaAt(endpoint, accountQuotaCallReason(readCtx, err, accountQuotaReasonReadFailed))
	}
	return accountQuotaReadResult{
		Available:  true,
		Generation: accountQuotaGenerationVerified,
		Limits:     limits,
		Backend:    endpoint,
		ObservedAt: time.Now().UTC(),
	}
}

func unavailableAccountQuota(reason string, generation accountQuotaGeneration) accountQuotaReadResult {
	return accountQuotaReadResult{Available: false, Reason: reason, Generation: generation}
}

// unavailableAccountQuotaAt marks a failure that occurred after the endpoint
// generation was verified, so callers can still compare generations.
func unavailableAccountQuotaAt(endpoint verifiedSharedEndpoint, reason string) accountQuotaReadResult {
	return accountQuotaReadResult{
		Available:  false,
		Reason:     reason,
		Generation: accountQuotaGenerationVerified,
		Backend:    endpoint,
	}
}

// accountQuotaEndpointOutcome classifies an endpoint-discovery failure into
// the diagnostics reason plus the generation verdict. Unsupported and
// ownership-unverifiable sources prove no usable endpoint generation exists;
// a spent deadline proves nothing. The verdict is decided here, at result
// construction — the cache never re-derives it from the reason string.
func accountQuotaEndpointOutcome(ctx context.Context, err error) (string, accountQuotaGeneration) {
	switch {
	case errors.Is(err, errSharedServerUnsupported):
		return accountQuotaReasonUnsupported, accountQuotaGenerationDisproved
	case errors.Is(err, errSharedServerOwnershipUnknown):
		return accountQuotaReasonUnverified, accountQuotaGenerationDisproved
	case accountQuotaDeadlineSpent(ctx, err):
		return accountQuotaReasonTimeout, accountQuotaGenerationInconclusive
	default:
		return accountQuotaReasonNotRunning, accountQuotaGenerationDisproved
	}
}

// accountQuotaDeadlineSpent treats a spent deadline uniformly across the
// discovery, dial, and read stages: a caller-supplied context error, a
// wrapped deadline, or an elapsed deadline all count. The deadline
// comparison covers the gap where the connection dies from a slammed socket
// deadline before the context timer marks the context done — the timeout
// must not flakily classify as a read failure.
func accountQuotaDeadlineSpent(ctx context.Context, err error) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return true
	}
	return false
}

// accountQuotaCallReason maps a call-stage failure to its diagnostics reason;
// the generation verdict is attached separately by the result constructor.
func accountQuotaCallReason(ctx context.Context, err error, fallback string) string {
	if accountQuotaDeadlineSpent(ctx, err) {
		return accountQuotaReasonTimeout
	}
	return fallback
}
