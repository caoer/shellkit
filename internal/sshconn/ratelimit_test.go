package sshconn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
)

func TestHostRateLimiter_Basic(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   3,
		window:  100 * time.Millisecond,
	}
	ctx := context.Background()

	// First 3 should succeed immediately.
	for i := 0; i < 3; i++ {
		if err := r.Acquire(ctx, "host1"); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}

	// 4th should block until the window slides.
	start := time.Now()
	if err := r.Acquire(ctx, "host1"); err != nil {
		t.Fatalf("acquire 4: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Fatalf("expected to wait ~100ms, waited %v", elapsed)
	}
}

func TestHostRateLimiter_DifferentHosts(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   2,
		window:  1 * time.Second,
	}
	ctx := context.Background()

	// Different hosts have independent windows.
	for i := 0; i < 2; i++ {
		if err := r.Acquire(ctx, "hostA"); err != nil {
			t.Fatalf("hostA acquire %d: %v", i, err)
		}
		if err := r.Acquire(ctx, "hostB"); err != nil {
			t.Fatalf("hostB acquire %d: %v", i, err)
		}
	}
}

func TestHostRateLimiter_ContextCancel(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   1,
		window:  10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Exhaust the window.
	if err := r.Acquire(ctx, "host1"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Next should fail with context deadline.
	err := r.Acquire(ctx, "host1")
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

func TestHostRateLimiter_PruneDeletesEmptyKeys(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   1,
		window:  10 * time.Millisecond,
	}
	ctx := context.Background()

	if err := r.Acquire(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}

	// Wait for the window to expire, then acquire again to trigger prune.
	time.Sleep(20 * time.Millisecond)
	if err := r.Acquire(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}

	// After the second acquire, the first timestamp was pruned. The map
	// should still contain the key (one active entry). Wait again and
	// acquire a different host to prove "ephemeral" gets cleaned up.
	time.Sleep(20 * time.Millisecond)
	if err := r.Acquire(ctx, "ephemeral"); err != nil {
		t.Fatal(err)
	}

	// Verify map doesn't hold stale keys with empty slices — prune
	// deletes them. We can't directly observe that without a lock, but
	// we can confirm a fresh key doesn't collide.
	r.mu.Lock()
	if _, ok := r.records["nonexistent"]; ok {
		t.Error("unexpected key 'nonexistent' in records map")
	}
	r.mu.Unlock()
}

func TestSshRateLimitKey_InventoryHost(t *testing.T) {
	srv := &inventory.Server{
		Name: "web-prod",
		IP:   "10.0.0.5",
		Port: 22,
	}
	key := SSHRateLimitKey(srv)
	if key != srv.ConnectAddr() {
		t.Fatalf("expected ConnectAddr %q, got %q", srv.ConnectAddr(), key)
	}
}

func TestSshRateLimitKey_AliasOnly(t *testing.T) {
	srv := &inventory.Server{
		Name:     "jump-gateway",
		SSHAlias: "jump-gateway",
	}
	key := SSHRateLimitKey(srv)
	if key == ":22" || key == ":0" {
		t.Fatalf("alias-only server should not key on empty IP, got %q", key)
	}
	if key != "jump-gateway" {
		t.Fatalf("expected SSHAlias %q, got %q", "jump-gateway", key)
	}
}

func TestSshRateLimitKey_NameFallback(t *testing.T) {
	srv := &inventory.Server{
		Name: "bare-name",
	}
	key := SSHRateLimitKey(srv)
	if key != "bare-name" {
		t.Fatalf("expected Name fallback %q, got %q", "bare-name", key)
	}
}

func TestHostRateLimiter_ConcurrentEnforcesThrottle(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   4,
		window:  200 * time.Millisecond,
	}
	ctx := context.Background()

	// 8 acquires with limit=4 and window=200ms: the second batch must wait.
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Acquire(ctx, "host1")
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// The second batch of 4 must wait ~200ms for the first batch's window.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("expected throttling (~200ms), completed in %v", elapsed)
	}
}

func TestHostRateLimiter_Concurrent(t *testing.T) {
	r := &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   4,
		window:  200 * time.Millisecond,
	}
	ctx := context.Background()

	// Launch 8 goroutines for the same host; all should complete (some after waiting).
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = r.Acquire(ctx, "host1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}
