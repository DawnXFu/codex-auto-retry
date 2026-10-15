package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// account_quota_reader_test.go pins the attach-only account quota reader
// (ticket #21 / spec #19): it attaches to an already-running verified shared
// app-server endpoint, performs one account/rateLimits/read over a
// short-lived connection, and reports every failure mode as a typed
// unavailable result instead of an error. The fake endpoint source is the
// discovery seam; the WebSocket double is the same fakeAppServer the shared
// controller tests use.

type staticAccountQuotaEndpointSource struct {
	endpoint verifiedSharedEndpoint
	err      error
	calls    int
}

func (s *staticAccountQuotaEndpointSource) VerifiedEndpoint(context.Context) (verifiedSharedEndpoint, error) {
	s.calls++
	return s.endpoint, s.err
}

func TestAccountQuotaReaderReadsRateLimitsFromVerifiedEndpoint(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0423")
	fake.rateLimitsResult = json.RawMessage(readAccountRateLimitsFixture(t, "account_rate_limits_read_full.json"))
	source := &staticAccountQuotaEndpointSource{endpoint: verifiedSharedEndpoint{
		Endpoint:  fake.endpoint(),
		PID:       4321,
		StartedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		CodexHome: `C:\Users\test\.codex`,
	}}

	result := newAccountQuotaReader(source).ReadAccountQuota(context.Background())
	if !result.Available {
		t.Fatalf("verified endpoint read reported unavailable: %q", result.Reason)
	}
	if result.AccountID() != "acc_8f3b2d91" {
		t.Fatalf("account generation handle: got %q", result.AccountID())
	}
	if result.Limits.RateLimits == nil || result.Limits.RateLimits.Primary == nil {
		t.Fatal("rateLimits.primary missing after attach-only read")
	}
	primary := result.Limits.RateLimits.Primary
	if primary.UsedPercent == nil || *primary.UsedPercent != 12 {
		t.Fatalf("primary.usedPercent: got %v", primary.UsedPercent)
	}
	if result.Backend != source.endpoint {
		t.Fatalf("backend generation handle changed: got %+v want %+v", result.Backend, source.endpoint)
	}
	if result.ObservedAt.IsZero() {
		t.Fatal("observed_at missing on a successful read")
	}

	// Attach-only means exactly handshake + one read: no lifecycle verbs, no
	// Ensure, no writes. The fake records only id-bearing requests.
	calls, _, providerRequests := fake.snapshot()
	if len(calls) != 2 || calls[0] != "initialize" || calls[1] != "account/rateLimits/read" {
		t.Fatalf("attach-only read sent unexpected requests: %v", calls)
	}
	if providerRequests != 0 {
		t.Fatalf("display read started provider work: %d", providerRequests)
	}

	// The connection is short-lived: the server sees it close after the read.
	waitFor(t, 2*time.Second, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.clients) == 0
	})
}

func TestAccountQuotaReaderReportsUnavailableWithoutEndpoint(t *testing.T) {
	source := &staticAccountQuotaEndpointSource{err: errors.New("shared-server.json missing")}
	result := newAccountQuotaReader(source).ReadAccountQuota(context.Background())
	if result.Available {
		t.Fatal("read without a verified endpoint must be unavailable")
	}
	if result.Reason != accountQuotaReasonNotRunning {
		t.Fatalf("missing endpoint reason: got %q", result.Reason)
	}
	if result.AccountID() != "" || !result.ObservedAt.IsZero() {
		t.Fatalf("unavailable result leaked evidence: %+v", result)
	}
	if source.calls != 1 {
		t.Fatalf("endpoint discovery ran %d times for one read", source.calls)
	}
}

func TestAccountQuotaReaderClassifiesVerificationFailures(t *testing.T) {
	for _, scenario := range []struct {
		err  error
		want string
	}{
		{errSharedServerUnsupported, accountQuotaReasonUnsupported},
		{errSharedServerOwnershipUnknown, accountQuotaReasonUnverified},
		{errSharedServerUnavailable, accountQuotaReasonNotRunning},
		{errors.New("corrupt shared-server.json"), accountQuotaReasonNotRunning},
	} {
		source := &staticAccountQuotaEndpointSource{err: scenario.err}
		result := newAccountQuotaReader(source).ReadAccountQuota(context.Background())
		if result.Available || result.Reason != scenario.want {
			t.Fatalf("endpoint error %v produced %+v, want reason %q", scenario.err, result, scenario.want)
		}
	}
}

func TestAccountQuotaReaderReportsUnreachableEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	source := &staticAccountQuotaEndpointSource{endpoint: verifiedSharedEndpoint{Endpoint: endpoint}}

	result := newAccountQuotaReader(source).ReadAccountQuota(context.Background())
	if result.Available || result.Reason != accountQuotaReasonUnreachable {
		t.Fatalf("dead endpoint produced %+v, want unreachable", result)
	}
}

func TestAccountQuotaReaderReportsReadFailure(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0424")
	// rateLimitsResult stays empty: the fake answers with a JSON-RPC error.
	source := &staticAccountQuotaEndpointSource{endpoint: verifiedSharedEndpoint{Endpoint: fake.endpoint()}}

	result := newAccountQuotaReader(source).ReadAccountQuota(context.Background())
	if result.Available || result.Reason != accountQuotaReasonReadFailed {
		t.Fatalf("failed account read produced %+v, want read failure", result)
	}
}

func TestAccountQuotaReaderBoundsDisplayReadWithTimeout(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0425")
	fake.rateLimitsResult = json.RawMessage(readAccountRateLimitsFixture(t, "account_rate_limits_read_full.json"))
	fake.rateLimitsDelay = 2 * time.Second
	source := &staticAccountQuotaEndpointSource{endpoint: verifiedSharedEndpoint{Endpoint: fake.endpoint()}}
	reader := &sharedAccountQuotaReader{source: source, timeout: 150 * time.Millisecond}

	started := time.Now()
	result := reader.ReadAccountQuota(context.Background())
	elapsed := time.Since(started)
	if result.Available || result.Reason != accountQuotaReasonTimeout {
		t.Fatalf("slow read produced %+v, want timeout", result)
	}
	if elapsed > time.Second {
		t.Fatalf("display read waited %v for a hung endpoint", elapsed)
	}
}

func TestAccountQuotaReaderHonorsCallerDeadlineDuringDiscovery(t *testing.T) {
	source := &blockingAccountQuotaEndpointSource{release: make(chan struct{})}
	defer close(source.release)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	result := newAccountQuotaReader(source).ReadAccountQuota(ctx)
	if result.Available || result.Reason != accountQuotaReasonTimeout {
		t.Fatalf("blocked discovery produced %+v, want timeout", result)
	}
}

func TestAccountQuotaReaderUnavailableWithoutSource(t *testing.T) {
	result := newAccountQuotaReader(nil).ReadAccountQuota(context.Background())
	if result.Available || result.Reason != accountQuotaReasonUnsupported {
		t.Fatalf("missing endpoint source produced %+v, want unsupported", result)
	}
}

func TestSharedControllerReadAccountQuotaAttachesWithoutEnsure(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0426")
	fake.rateLimitsResult = json.RawMessage(readAccountRateLimitsFixture(t, "account_rate_limits_read_full.json"))
	server := &attachOnlySharedServer{
		staticSharedServer: staticSharedServer{endpoint: fake.endpoint(), home: `C:\Users\test\.codex`},
		verified:           verifiedSharedEndpoint{Endpoint: fake.endpoint(), PID: 77},
	}
	controller := &sharedAppServerController{server: server}

	result := controller.ReadAccountQuota(context.Background())
	if !result.Available || result.Backend.PID != 77 {
		t.Fatalf("controller read failed: %+v", result)
	}
	if server.ensureCalls != 0 {
		t.Fatalf("display read invoked an Ensure/start path %d times", server.ensureCalls)
	}
}

func TestSharedControllerReadAccountQuotaUnsupportedWithoutVerifier(t *testing.T) {
	fake := newFakeAppServer(t, "019f9d5d-9c82-75b1-b7c0-20a658af0427")
	controller := &sharedAppServerController{
		server: staticSharedServer{endpoint: fake.endpoint(), home: `C:\Users\test\.codex`},
	}
	result := controller.ReadAccountQuota(context.Background())
	if result.Available || result.Reason != accountQuotaReasonUnsupported {
		t.Fatalf("unverifiable server produced %+v, want unsupported", result)
	}
	if calls, _, _ := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("unsupported read still dialed the endpoint: %v", calls)
	}
}

// attachOnlySharedServer is a sharedServer that also satisfies
// accountQuotaEndpointSource; it counts Ensure calls so tests can prove the
// quota read is attach-only.
type attachOnlySharedServer struct {
	staticSharedServer
	verified    verifiedSharedEndpoint
	verifiedErr error
	ensureCalls int
}

func (s *attachOnlySharedServer) Ensure(context.Context) error {
	s.ensureCalls++
	return nil
}

func (s *attachOnlySharedServer) VerifiedEndpoint(context.Context) (verifiedSharedEndpoint, error) {
	return s.verified, s.verifiedErr
}

func TestAppResumeRunnerExposesAccountQuotaReaderOptionally(t *testing.T) {
	// Mirrors the SharedBackendMemory/RetryThreadStatus seam: runners without
	// a shared backend report unavailable instead of failing to compile.
	runner := &appResumeRunner{controller: staticControllerNoQuota{}}
	result := runner.ReadAccountQuota(context.Background())
	if result.Available || result.Reason != accountQuotaReasonUnsupported {
		t.Fatalf("unsupported runner produced %+v", result)
	}
}

type blockingAccountQuotaEndpointSource struct {
	release chan struct{}
}

func (s *blockingAccountQuotaEndpointSource) VerifiedEndpoint(ctx context.Context) (verifiedSharedEndpoint, error) {
	select {
	case <-s.release:
		return verifiedSharedEndpoint{}, errors.New("released without endpoint")
	case <-ctx.Done():
		return verifiedSharedEndpoint{}, ctx.Err()
	}
}

// staticControllerNoQuota is a recovery controller stub that deliberately
// does not implement accountQuotaReader.
type staticControllerNoQuota struct{}

func (staticControllerNoQuota) Prepare(context.Context) error { return nil }
func (staticControllerNoQuota) Readiness(context.Context) (string, error) {
	return "ready", nil
}
func (staticControllerNoQuota) Dispatch(context.Context, string, string, ResumeSettings, time.Time, time.Time, string, bool, bool, FailureClass, string) (DispatchResult, error) {
	return DispatchResult{}, nil
}
func (staticControllerNoQuota) BlockGoal(context.Context, string, *ResumeSettings, string) (DispatchResult, error) {
	return DispatchResult{}, nil
}
