package gateway

import (
	"testing"

	"github.com/zereker/llm-gateway/internal/config"
	"github.com/zereker/llm-gateway/internal/ratelimit"
)

// TestSchedulerFilters_ConfigWiringSync guards the duplicated selector-filter
// allowlist: config.Validate accepts a set of names, and buildSchedulerFilters
// has a switch that must handle every one of them (an accepted-but-unhandled
// name panics at gateway startup). The two lists live in different packages
// and are maintained by hand — this test makes "config accepts it" imply
// "wiring can build it".
func TestSchedulerFilters_ConfigWiringSync(t *testing.T) {
	store := ratelimit.NewInMemoryStore()

	for _, name := range config.ValidSelectorFilters() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("config accepts selector filter %q but buildSchedulerFilters panics: %v", name, r)
				}
			}()

			// nil cooldown is fine: this only exercises construction, not filtering.
			buildSchedulerFilters([]string{name}, store, nil)
		}()
	}
}

// TestBuildRateLimitStore_CoversConfigDrivers mirrors the same guarantee for
// the rate_limit.driver switch: every driver value config.Validate accepts
// must construct (redis may construct against a nil client — only the
// constructor runs here).
func TestBuildRateLimitStore_CoversConfigDrivers(t *testing.T) {
	for _, driver := range []string{"", "redis", "inmemory"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("config accepts rate_limit.driver=%q but buildRateLimitStore panics: %v", driver, r)
				}
			}()

			if s := buildRateLimitStore(config.RateLimitConfig{Driver: driver}, nil); s == nil {
				t.Errorf("rate_limit.driver=%q built a nil store", driver)
			}
		}()
	}
}
