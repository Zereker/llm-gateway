// Package cachebus is the **narrow cache-invalidation channel** (Redis pub/sub)
// from the control plane to the data plane.
//
// **Why it exists**: the two planes default to passive sync via MySQL + the data
// plane's TTL cache (≤30s) — that's good enough for **functional** changes like
// adding an endpoint or changing a price. But api_key **revocation** is a
// **security** event: letting a leaked key stay valid for another 30s is
// unacceptable. cachebus opens one precise channel just for this kind of
// security-sensitive invalidation: the control plane PUBLISHes an
// `apikey:<hash>` message, the data plane's subscriber evicts that single key,
// shrinking the window from ≤30s down to sub-second.
//
// **Deliberately narrow**: this is **not** a general-purpose cache-coherence
// protocol — it only broadcasts "this key/resource is invalidated, whoever
// cached it should delete it." No versioning, no full refresh, and it never
// touches the auth hot path (auth still goes through the local LRU; only
// invalidation events touch Redis pub/sub). If Redis is unavailable it
// degrades to plain TTL (logs a warn), without blocking either plane.
//
// **Known narrow window (evict-without-version)**: an in-flight Resolve that
// started **before** revocation but completes **after** the evict may write an
// identity that was "still valid at the time" back into the cache, leaving a
// stale entry for up to one forward TTL (30s). The window is the duration of a
// single Resolve call straddling the revocation — narrow, but real. Closing it
// completely would require adding a generation/version token to cache entries;
// for now the 30s TTL serves as the backstop, which is an acceptable trade-off.
package cachebus

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// DefaultChannel is the default Redis pub/sub channel name.
const DefaultChannel = "llm-gateway:cache:invalidate"

// Kind marks the type of resource being invalidated. Phase 0/1 only has apikey;
// endpoint / subscription can be added later.
type Kind string

const (
	// KindAPIKey: Key is api_key_hash (SHA-256 hex).
	KindAPIKey Kind = "apikey"
)

// Invalidation is a single invalidation message. Wire format: `<kind>:<key>`.
type Invalidation struct {
	Kind Kind
	Key  string
}

func (inv Invalidation) encode() string { return string(inv.Kind) + ":" + inv.Key }

func decode(payload string) (Invalidation, bool) {
	i := strings.IndexByte(payload, ':')
	if i <= 0 || i == len(payload)-1 {
		return Invalidation{}, false
	}

	return Invalidation{Kind: Kind(payload[:i]), Key: payload[i+1:]}, true
}

// Publisher sends messages to the invalidation channel (used by the control plane).
type Publisher struct {
	rdb     *redis.Client
	channel string
}

// NewPublisher constructs a Publisher; an empty channel falls back to DefaultChannel.
func NewPublisher(rdb *redis.Client, channel string) *Publisher {
	if channel == "" {
		channel = DefaultChannel
	}

	return &Publisher{rdb: rdb, channel: channel}
}

// Invalidate PUBLISHes a single invalidation message. Best-effort: if Redis is
// down, the returned error is logged as a warn by the caller and does not block
// the control-plane write (the DB write has already landed; the TTL fallback
// provides eventual consistency).
func (p *Publisher) Invalidate(ctx context.Context, inv Invalidation) error {
	if p == nil || p.rdb == nil {
		return nil
	}

	if err := p.rdb.Publish(ctx, p.channel, inv.encode()).Err(); err != nil {
		return fmt.Errorf("cachebus: publish: %w", err)
	}

	return nil
}

// Subscriber subscribes to the invalidation channel and dispatches each
// message to handler (used by the data plane).
type Subscriber struct {
	rdb     *redis.Client
	channel string
	handler func(Invalidation)
}

// NewSubscriber constructs a Subscriber; an empty channel falls back to DefaultChannel.
func NewSubscriber(rdb *redis.Client, channel string, handler func(Invalidation)) *Subscriber {
	if channel == "" {
		channel = DefaultChannel
	}

	return &Subscriber{rdb: rdb, channel: channel, handler: handler}
}

// Start subscribes to the channel and spawns a background goroutine to
// dispatch messages. It only returns after **synchronously confirming** the
// subscription succeeded — once it returns, any subsequent Publish is
// guaranteed to be delivered (letting tests assert deterministically). The
// returned stop closes the subscription and ends the goroutine.
func (s *Subscriber) Start(ctx context.Context) (stop func(), err error) {
	pubsub := s.rdb.Subscribe(ctx, s.channel)
	// One Receive call confirms the subscription is established
	// (*redis.Subscription); failure means Redis is unavailable.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, fmt.Errorf("cachebus: subscribe: %w", err)
	}

	ch := pubsub.Channel()

	done := make(chan struct{})
	go func() {
		defer close(done)

		for msg := range ch {
			if inv, ok := decode(msg.Payload); ok && s.handler != nil {
				s.handler(inv)
			}
		}
	}()

	return func() {
		_ = pubsub.Close()

		<-done
	}, nil
}
