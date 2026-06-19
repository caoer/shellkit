package sshconn

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
)

// SSHRateLimit is the global per-host SSH connection rate limiter.
// It prevents connection storms that trigger hosting provider abuse detection
// (e.g., BandwagonHost suspends VMs for "too many SSH attempts in 180s").
//
// Default: 16 connections per 30-second sliding window per host.
// Override via SHELLKIT_RATE_LIMIT="limit/windowSecs" (e.g., "10/60").
var SSHRateLimit = newHostRateLimiter("SHELLKIT_RATE_LIMIT")

// SSHRateLimitKey returns a stable key for rate-limiting SSH connections to
// srv. Uses ConnectAddr() for inventory hosts (resolved IP:port), but falls
// back to Name or SSHAlias for alias-only servers whose ResolvedIP() is empty
// — avoids all aliases sharing the single key ":22".
func SSHRateLimitKey(srv *inventory.Server) string {
	if ip := srv.ResolvedIP(); ip != "" {
		return srv.ConnectAddr()
	}
	if srv.SSHAlias != "" {
		return srv.SSHAlias
	}
	return srv.Name
}

type hostRateLimiter struct {
	mu      sync.Mutex
	records map[string][]time.Time
	limit   int
	window  time.Duration
}

func newHostRateLimiter(envVar string) *hostRateLimiter {
	limit := 16
	window := 30 * time.Second

	if env := os.Getenv(envVar); env != "" {
		if parts := strings.SplitN(env, "/", 2); len(parts) == 2 {
			if l, err := strconv.Atoi(parts[0]); err == nil && l > 0 {
				limit = l
			}
			if w, err := strconv.Atoi(parts[1]); err == nil && w > 0 {
				window = time.Duration(w) * time.Second
			}
		}
	}

	return &hostRateLimiter{
		records: make(map[string][]time.Time),
		limit:   limit,
		window:  window,
	}
}

// prune removes entries older than the window for the given host.
func (r *hostRateLimiter) prune(host string, now time.Time) {
	cutoff := now.Add(-r.window)
	entries := r.records[host]
	i := 0
	for i < len(entries) && entries[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		remaining := entries[i:]
		if len(remaining) == 0 {
			delete(r.records, host)
		} else {
			r.records[host] = remaining
		}
	}
}

// Acquire blocks until it is safe to open an SSH connection to host.
// Returns ctx.Err() if the context is cancelled while waiting.
func (r *hostRateLimiter) Acquire(ctx context.Context, host string) error {
	for {
		r.mu.Lock()
		now := time.Now()
		r.prune(host, now)

		if len(r.records[host]) < r.limit {
			r.records[host] = append(r.records[host], now)
			r.mu.Unlock()
			return nil
		}

		// Calculate when the oldest entry expires out of the window.
		oldest := r.records[host][0]
		waitUntil := oldest.Add(r.window)
		r.mu.Unlock()

		wait := time.Until(waitUntil)
		if wait <= 0 {
			continue
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
