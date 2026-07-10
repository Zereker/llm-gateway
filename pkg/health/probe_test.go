package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zereker/llm-gateway/pkg/domain"
	"github.com/zereker/llm-gateway/pkg/selector"
)

type staticSource struct{ eps []*domain.Endpoint }

func (s staticSource) ListProbeTargets(context.Context) ([]*domain.Endpoint, error) {
	return s.eps, nil
}

type recordingStats struct {
	mu      sync.Mutex
	results map[int64][]selector.Result
}

func (r *recordingStats) Record(_ context.Context, id int64, res selector.Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.results == nil {
		r.results = make(map[int64][]selector.Result)
	}
	r.results[id] = append(r.results[id], res)
}

func (r *recordingStats) Snapshot(_ context.Context, _ int64) selector.EndpointStats {
	return selector.EndpointStats{}
}

type fakeCooldown struct {
	mu      sync.Mutex
	cooled  map[int64]bool
	cleared []int64
}

func (f *fakeCooldown) Mark(_ context.Context, id int64, _ selector.ErrorClass, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cooled == nil {
		f.cooled = make(map[int64]bool)
	}
	f.cooled[id] = true
	return nil
}

func (f *fakeCooldown) InCooldown(_ context.Context, ids []int64) (map[int64]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if f.cooled[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeCooldown) Clear(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cooled, id)
	f.cleared = append(f.cleared, id)
	return nil
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

// A successful probe of a cooling endpoint must clear its cooldown early.
func TestProber_RecoverCooldownOnSuccessfulProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cd := &fakeCooldown{cooled: map[int64]bool{7: true}}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(7, srv.URL)}},
		Stats:    &recordingStats{},
		Cooldown: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.cleared) != 1 || cd.cleared[0] != 7 {
		t.Fatalf("expected cooldown cleared for endpoint 7, got %v", cd.cleared)
	}
	if cd.cooled[7] {
		t.Error("endpoint 7 should no longer be cooling")
	}
}

// A failed probe must NOT clear the cooldown (release-only on success).
func TestProber_FailedProbeKeepsCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cd := &fakeCooldown{cooled: map[int64]bool{7: true}}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(7, srv.URL)}},
		Stats:    &recordingStats{},
		Cooldown: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	defer cd.mu.Unlock()
	if len(cd.cleared) != 0 {
		t.Fatalf("failed probe must not clear cooldown, cleared=%v", cd.cleared)
	}
	if !cd.cooled[7] {
		t.Error("endpoint 7 should still be cooling")
	}
}

// A healthy endpoint that was never cooling must not trigger Clear calls.
func TestProber_HealthyEndpointNotCleared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cd := &fakeCooldown{}
	stats := &recordingStats{}
	p := New(Config{
		Source:   staticSource{eps: []*domain.Endpoint{probeEndpoint(9, srv.URL)}},
		Stats:    stats,
		Cooldown: cd,
	})

	p.cycle(context.Background())

	cd.mu.Lock()
	if len(cd.cleared) != 0 {
		t.Errorf("no cooldown existed; Clear should not be called, got %v", cd.cleared)
	}
	cd.mu.Unlock()

	stats.mu.Lock()
	defer stats.mu.Unlock()
	if len(stats.results[9]) != 1 || stats.results[9][0].Class != selector.ClassSuccess {
		t.Errorf("probe should still record stats, got %+v", stats.results[9])
	}
}
