package provider

// Per-endpoint concurrency limiting. Multiple executor sessions (and the
// orchestrator) often point at the same local model server, which serves
// requests serially — firing them concurrently gains nothing and just adds
// contention and timeout risk. A shared semaphore keyed by endpoint caps how
// many completions are in flight at once: local endpoints default to 1 (truly
// serial), cloud endpoints are unlimited (they handle concurrency and the
// orchestrator already bounds batch size). Distinct endpoints keep their own
// limiters, so real parallelism across different backends is preserved.

import (
	"context"
	"strings"
	"sync"

	"github.com/TedHaley/cheep/internal/core"
)

var (
	limMu   sync.Mutex
	limSems = map[string]chan struct{}{} // endpoint key -> semaphore (nil = unlimited)
	limSet  = map[string]bool{}          // endpoint key -> limit resolved
	limOver = map[string]int{}           // endpoint key -> configured override (<=0 = unlimited)
)

// SetEndpointLimit overrides the max concurrent in-flight completions for an
// endpoint (n <= 0 means unlimited). Call at startup, before providers run.
func SetEndpointLimit(endpoint string, n int) {
	limMu.Lock()
	limOver[normEndpoint(endpoint)] = n
	limMu.Unlock()
}

func normEndpoint(baseURL string) string {
	b := strings.TrimRight(baseURL, "/")
	if b == "" {
		return "anthropic" // Anthropic's default endpoint
	}
	return b
}

func isLocalEndpoint(b string) bool {
	return strings.Contains(b, "localhost") || strings.Contains(b, "127.0.0.1") ||
		strings.Contains(b, "0.0.0.0") || strings.Contains(b, "[::1]")
}

// semFor returns the endpoint's semaphore, or nil when unlimited. The limit is
// resolved once per endpoint: an explicit override wins, else local = 1 and
// everything else is unlimited.
func semFor(key string) chan struct{} {
	limMu.Lock()
	defer limMu.Unlock()
	if limSet[key] {
		return limSems[key]
	}
	limit := 0 // unlimited (cloud / remote)
	if n, ok := limOver[key]; ok {
		limit = n
	} else if key != "anthropic" && isLocalEndpoint(key) {
		limit = 1 // a local model serves serially: one request at a time
	}
	var s chan struct{}
	if limit > 0 {
		s = make(chan struct{}, limit)
	}
	limSems[key] = s
	limSet[key] = true
	return s
}

// limited wraps a provider, capping concurrent Complete calls per endpoint.
type limited struct {
	inner core.Provider
	key   string
}

func (l limited) Complete(ctx context.Context, model, system string, msgs []core.Message, tools []core.Tool) (core.Turn, error) {
	if s := semFor(l.key); s != nil {
		select {
		case s <- struct{}{}:
			defer func() { <-s }()
		case <-ctx.Done():
			return core.Turn{}, ctx.Err()
		}
	}
	return l.inner.Complete(ctx, model, system, msgs, tools)
}
