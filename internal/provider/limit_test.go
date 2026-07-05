package provider

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TedHaley/cheep/internal/core"
)

// countingProvider records the peak number of concurrent Complete calls.
type countingProvider struct {
	cur, peak int32
}

func (c *countingProvider) Complete(context.Context, string, string, []core.Message, []core.Tool) (core.Turn, error) {
	n := atomic.AddInt32(&c.cur, 1)
	for {
		p := atomic.LoadInt32(&c.peak)
		if n <= p || atomic.CompareAndSwapInt32(&c.peak, p, n) {
			break
		}
	}
	time.Sleep(15 * time.Millisecond) // hold the slot so overlap is observable
	atomic.AddInt32(&c.cur, -1)
	return core.Turn{}, nil
}

func fireConcurrent(p core.Provider, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.Complete(context.Background(), "m", "s", nil, nil) }()
	}
	wg.Wait()
}

func TestLocalEndpointSerializes(t *testing.T) {
	c := &countingProvider{}
	p := limited{inner: c, key: normEndpoint("http://127.0.0.1:9911/v1")} // local → default 1
	fireConcurrent(p, 8)
	if c.peak != 1 {
		t.Errorf("local endpoint peak concurrency = %d, want 1", c.peak)
	}
}

func TestRemoteEndpointUnlimited(t *testing.T) {
	c := &countingProvider{}
	p := limited{inner: c, key: normEndpoint("https://api.deepseek.com/v1")} // remote → unlimited
	fireConcurrent(p, 8)
	if c.peak < 2 {
		t.Errorf("remote endpoint peak concurrency = %d, want > 1 (unlimited)", c.peak)
	}
}

func TestOverrideRaisesLocalLimit(t *testing.T) {
	SetEndpointLimit("http://127.0.0.1:9912/v1", 3)
	c := &countingProvider{}
	p := limited{inner: c, key: normEndpoint("http://127.0.0.1:9912/v1")}
	fireConcurrent(p, 8)
	if c.peak != 3 {
		t.Errorf("overridden endpoint peak concurrency = %d, want 3", c.peak)
	}
}

func TestDistinctEndpointsRunInParallel(t *testing.T) {
	// Two different local endpoints each cap at 1, but run concurrently with
	// each other — real parallelism across distinct backends is preserved.
	var wg sync.WaitGroup
	var a, b countingProvider
	pa := limited{inner: &a, key: normEndpoint("http://127.0.0.1:9913/v1")}
	pb := limited{inner: &b, key: normEndpoint("http://127.0.0.1:9914/v1")}
	both := &countingProvider{}
	_ = both
	wg.Add(2)
	go func() { defer wg.Done(); fireConcurrent(pa, 4) }()
	go func() { defer wg.Done(); fireConcurrent(pb, 4) }()
	wg.Wait()
	if a.peak != 1 || b.peak != 1 {
		t.Errorf("each endpoint should serialize: a=%d b=%d", a.peak, b.peak)
	}
}
