package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/failure"
)

type staticSource struct{ eps []*domain.Endpoint }

func (s staticSource) ListProbeTargets(context.Context) ([]*domain.Endpoint, error) {
	return s.eps, nil
}

type recordingStats struct {
	mu      sync.Mutex
	results map[int64][]Result
}

func (r *recordingStats) RecordHealth(_ context.Context, id int64, res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = make(map[int64][]Result)
	}
	r.results[id] = append(r.results[id], res)
}

// fakeCooldown models a cooldown store keyed by class. ClearIfRecoverable only
// releases Transient/Capacity, mirroring the Redis Lua behavior.
type fakeCooldown struct {
	mu      sync.Mutex
	cooled  map[int64]failure.Class
	cleared []int64
}

func (f *fakeCooldown) Mark(_ context.Context, id int64, class failure.Class, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cooled == nil {
		f.cooled = make(map[int64]failure.Class)
	}
	f.cooled[id] = class
	return nil
}

func (f *fakeCooldown) InCooldown(_ context.Context, ids []int64) (map[int64]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if _, ok := f.cooled[id]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeCooldown) ClearIfRecoverable(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	class, ok := f.cooled[id]
	if !ok {
		return false, nil
	}
	if class != failure.Transient && class != failure.Capacity {
		return false, nil // permanent/invalid/unknown: not probe-recoverable
	}
	delete(f.cooled, id)
	f.cleared = append(f.cleared, id)
	return true, nil
}

func probeEndpoint(id int64, url string) *domain.Endpoint {
	return &domain.Endpoint{
		ID: id,
		Capabilities: domain.EndpointCapabilities{
			SelfHosted:          true,
			HealthProbeEndpoint: url,
		},
	}
}

// A successful probe of a Transient-cooling endpoint releases it early.
func TestProber_RecoverTransientCooldownOnSuccessfulProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cd := &fakeCooldown{cooled: map[int64]failure.Class{7: failure.Transient}}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(7, srv.URL)}},
		Feedback: &recordingStats{},
		Recovery: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.cleared) != 1 || cd.cleared[0] != 7 {
		t.Fatalf("expected cooldown cleared for endpoint 7, got %v", cd.cleared)
	}
	if _, ok := cd.cooled[7]; ok {
		t.Error("endpoint 7 should no longer be cooling")
	}
}

// A successful probe must NOT release a Permanent (bad-credential) cooldown —
// a health-200 does not attest the API key is valid.
func TestProber_PermanentCooldownNotRecovered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cd := &fakeCooldown{cooled: map[int64]failure.Class{7: failure.Permanent}}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(7, srv.URL)}},
		Feedback: &recordingStats{},
		Recovery: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.cleared) != 0 {
		t.Fatalf("permanent cooldown must not be recovered, cleared=%v", cd.cleared)
	}
	if cd.cooled[7] != failure.Permanent {
		t.Error("endpoint 7 should still hold its permanent cooldown")
	}
}

// A failed probe must NOT clear the cooldown (release-only on success).
func TestProber_FailedProbeKeepsCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cd := &fakeCooldown{cooled: map[int64]failure.Class{7: failure.Transient}}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(7, srv.URL)}},
		Feedback: &recordingStats{},
		Recovery: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.cleared) != 0 {
		t.Fatalf("failed probe must not clear cooldown, cleared=%v", cd.cleared)
	}
	if cd.cooled[7] != failure.Transient {
		t.Error("endpoint 7 should still be cooling")
	}
}

// A healthy endpoint that was never cooling records stats and triggers no clear.
func TestProber_HealthyEndpointNotCleared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cd := &fakeCooldown{}
	stats := &recordingStats{}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(9, srv.URL)}},
		Feedback: stats,
		Recovery: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	if len(cd.cleared) != 0 {
		t.Errorf("no cooldown existed; nothing should be cleared, got %v", cd.cleared)
	}
	cd.mu.Unlock()

	stats.mu.Lock()
	defer stats.mu.Unlock()
	if len(stats.results[9]) != 1 || stats.results[9][0].Class != failure.Success {
		t.Errorf("probe should still record stats, got %+v", stats.results[9])
	}
}
