package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildTmuxBody_Structure(t *testing.T) {
	verbs := []TmuxVerb{
		{Wire: "spawn_b64 dGVzdA=="},
		{Wire: "snap lines=200"},
		{Wire: "kill"},
	}

	body, err := buildTmuxBody("mysession", "testnonce", verbs)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(body, "do_spawn()") {
		t.Error("body should contain do_spawn function")
	}
	if !strings.Contains(body, "do_snap()") {
		t.Error("body should contain do_snap function")
	}
	if !strings.Contains(body, "SESS='mysession'") {
		t.Error("body should set SESS to session name")
	}
	if !strings.Contains(body, "# nonce: testnonce") {
		t.Error("body should contain nonce comment")
	}
	if !strings.Contains(body, "<<'SHELLKIT_VERBS_testnonc'") {
		t.Error("body should contain nonce-based heredoc redirect")
	}
	if !strings.Contains(body, "spawn_b64 dGVzdA==") {
		t.Error("body should contain spawn verb wire")
	}
	if !strings.Contains(body, "snap lines=200") {
		t.Error("body should contain snap verb wire")
	}
	if !strings.Contains(body, "\nSHELLKIT_VERBS_testnonc\n") {
		t.Error("body should end with nonce-based delimiter on own line")
	}
}

func TestBuildTmuxBody_WrappedScript(t *testing.T) {
	verbs := []TmuxVerb{
		{Wire: "spawn_b64 dGVzdA=="},
		{Wire: "snap lines=200"},
	}

	body, err := buildTmuxBody("sess", "nonce123", verbs)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := wrapScript(body, "bash", "nonce123")

	if !strings.Contains(wrapped, "_SSH_OUTPUT=$(mktemp)") {
		t.Error("wrapped should have mktemp for output")
	}
	if !strings.Contains(wrapped, outputMarkerFor("nonce123")) {
		t.Error("wrapped should have output marker")
	}
	if !strings.Contains(wrapped, stderrMarkerFor("nonce123")) {
		t.Error("wrapped should have stderr marker")
	}
	if !strings.Contains(wrapped, "bash $_SSH_SCRIPT") {
		t.Error("wrapped should invoke bash on script file")
	}
	if !strings.Contains(wrapped, "do_spawn()") {
		t.Error("wrapped should contain interpreter functions")
	}
	if !strings.Contains(wrapped, "SHELLKIT_VERBS") {
		t.Error("wrapped should contain verb heredoc")
	}
}

func TestExecuteTmux_ParseError(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "bad-verbs",
		Action: ActionTmux,
		Hosts:  []string{"myhost:sess"},
		Body:   "badverb arg1 arg2",
	}
	_, err := ex.executeTmux(context.Background(), 0, step)
	if err == nil {
		t.Fatal("should error on invalid verb body")
	}
	if !strings.Contains(err.Error(), "unknown verb") {
		t.Errorf("error should mention unknown verb, got: %s", err)
	}
	if !strings.Contains(err.Error(), "bad-verbs") {
		t.Errorf("error should mention step name, got: %s", err)
	}
}

func TestExecuteTmux_UnknownHost(t *testing.T) {
	// Plain hostnames now fall back to ssh_config resolution, so the only
	// pre-flight rejection is for inputs that don't look like host names.
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "bad-host",
		Action: ActionTmux,
		Hosts:  []string{"not a host:sess"},
		Body:   "snap",
	}
	_, err := ex.executeTmux(context.Background(), 0, step)
	if err == nil {
		t.Fatal("should error on malformed host")
	}
	if !strings.Contains(err.Error(), "unknown host") {
		t.Errorf("error should mention unknown host, got: %s", err)
	}
}

func TestExecuteTmux_NoTargets(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{Name: "no-targets", Action: ActionTmux, Body: "snap"}
	_, err := ex.executeTmux(context.Background(), 0, step)
	if err == nil {
		t.Fatal("should error when no targets")
	}
	if !strings.Contains(err.Error(), "at least one target") {
		t.Errorf("error should mention targets required, got: %s", err)
	}
}

func TestVerbStreamConstruction(t *testing.T) {
	verbs := []TmuxVerb{
		{Wire: "spawn_b64 dGVzdA=="},
		{Wire: "expect aGVsbG8= 30"},
		{Wire: "snap lines=200"},
		{Wire: "kill"},
	}

	body, err := buildTmuxBody("sess", "nonce", verbs)
	if err != nil {
		t.Fatal(err)
	}

	// Extract verb stream between heredoc markers
	marker := "<<'SHELLKIT_VERBS_nonce'\n"
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("no SHELLKIT_VERBS_nonce heredoc found")
	}
	start += len(marker)
	end := strings.Index(body[start:], "\nSHELLKIT_VERBS_nonce\n")
	if end < 0 {
		t.Fatal("no SHELLKIT_VERBS_nonce end marker found")
	}
	verbStream := body[start : start+end]

	lines := strings.Split(verbStream, "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 verb lines, got %d: %q", len(lines), verbStream)
	}

	expected := []string{
		"spawn_b64 dGVzdA==",
		"expect aGVsbG8= 30",
		"snap lines=200",
		"kill",
	}
	for i, want := range expected {
		if lines[i] != want {
			t.Errorf("line %d: want %q, got %q", i, want, lines[i])
		}
	}
}

func TestBuildTmuxBody_EmptyVerbs(t *testing.T) {
	body, err := buildTmuxBody("sess", "nonce1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "<<'SHELLKIT_VERBS_nonce123'") {
		t.Error("body should still contain heredoc even with empty verbs")
	}
	if !strings.Contains(body, "SHELLKIT_VERBS_nonce123") {
		t.Error("body should have closing delimiter")
	}
}

// --- Fan-out output resolution (#2) ---

func TestFanoutOutputResolution(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	r1 := StepResult{
		Name: "deploy", Host: "h1:s1", ExitCode: 0,
		Stdout:  "output from h1",
		Outputs: map[string]string{"snap.0": "aaa"},
	}
	r2 := StepResult{
		Name: "deploy", Host: "h2:s2", ExitCode: 0,
		Stdout:  "output from h2",
		Outputs: map[string]string{"snap.0": "bbb"},
	}

	store.Store(&r1)
	store.Store(&r2)

	merged := ex.mergeFanoutOutputs("deploy", []StepResult{r1, r2})
	store.Store(&merged)

	// per-host resolution
	val, err := store.resolveExpr("deploy.h1:s1.outputs.snap.0")
	if err != nil {
		t.Fatalf("resolve h1:s1: %v", err)
	}
	if val != "aaa" {
		t.Errorf("h1:s1.snap.0: want aaa, got %q", val)
	}

	val, err = store.resolveExpr("deploy.h2:s2.outputs.snap.0")
	if err != nil {
		t.Fatalf("resolve h2:s2: %v", err)
	}
	if val != "bbb" {
		t.Errorf("h2:s2.snap.0: want bbb, got %q", val)
	}

	// merged output file path
	val, err = store.resolveExpr("deploy.output")
	if err != nil {
		t.Fatalf("resolve merged output: %v", err)
	}
	if val == "" {
		t.Error("merged output file path should not be empty")
	}
}

// --- Fan-out concurrency tests (#6) ---

func TestFanout_CancelOnFirstError(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "test-cancel",
		Config: StepConfig{Timeout: 10},
	}
	targets := []string{"h1:s1", "h2:s2", "h3:s3"}

	var t3ctx context.Context
	runner := func(ctx context.Context, target string) (StepResult, error) {
		switch target {
		case "h1:s1":
			time.Sleep(50 * time.Millisecond)
			return StepResult{Name: step.Name, Host: target, ExitCode: 0, Stdout: "ok"}, nil
		case "h2:s2":
			return StepResult{Name: step.Name, Host: target, ExitCode: 1, Error: "connection refused"}, nil
		case "h3:s3":
			t3ctx = ctx
			<-ctx.Done()
			return StepResult{Name: step.Name, Host: target, ExitCode: 1, Error: ctx.Err().Error()}, nil
		}
		return StepResult{}, fmt.Errorf("unexpected target %s", target)
	}

	results, err := ex.executeTmuxFanout(context.Background(), step, targets, runner)
	if err != nil {
		t.Fatalf("executeTmuxFanout: %v", err)
	}

	// h2 should have error
	found := false
	for _, r := range results {
		if r.Host == "h2:s2" {
			found = true
			if r.ExitCode != 1 {
				t.Errorf("h2 exit code: want 1, got %d", r.ExitCode)
			}
		}
	}
	if !found {
		t.Error("h2:s2 result missing")
	}

	// h3's context should have been cancelled
	if t3ctx == nil {
		t.Fatal("h3 context was never set")
	}
	if t3ctx.Err() == nil {
		t.Error("h3 context should be cancelled")
	}
}

func TestFanout_ContinueOnError(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "test-continue",
		Config: StepConfig{Timeout: 10, ContinueOnError: true},
	}
	targets := []string{"h1:s1", "h2:s2", "h3:s3"}

	runner := func(ctx context.Context, target string) (StepResult, error) {
		if target == "h2:s2" {
			return StepResult{Name: step.Name, Host: target, ExitCode: 1, Error: "fail"}, nil
		}
		time.Sleep(20 * time.Millisecond)
		return StepResult{Name: step.Name, Host: target, ExitCode: 0, Stdout: "ok"}, nil
	}

	results, err := ex.executeTmuxFanout(context.Background(), step, targets, runner)
	if err != nil {
		t.Fatalf("executeTmuxFanout: %v", err)
	}

	// Should have 3 per-host + 1 merged = 4 results
	if len(results) != 4 {
		t.Fatalf("want 4 results (3 per-host + merged), got %d", len(results))
	}

	okCount := 0
	errCount := 0
	for _, r := range results[:3] {
		if r.ExitCode == 0 {
			okCount++
		} else {
			errCount++
		}
	}
	if okCount != 2 {
		t.Errorf("want 2 successes, got %d", okCount)
	}
	if errCount != 1 {
		t.Errorf("want 1 failure, got %d", errCount)
	}
}

func TestFanout_SemaphoreBounds(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "test-sem",
		Config: StepConfig{Timeout: 30, ContinueOnError: true},
	}

	const numTargets = 50
	targets := make([]string, numTargets)
	for i := range targets {
		targets[i] = fmt.Sprintf("h%d:s%d", i, i)
	}

	var concurrent int64
	var maxConcurrent int64

	runner := func(ctx context.Context, target string) (StepResult, error) {
		cur := atomic.AddInt64(&concurrent, 1)
		for {
			old := atomic.LoadInt64(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt64(&concurrent, -1)
		return StepResult{Name: step.Name, Host: target, ExitCode: 0}, nil
	}

	results, err := ex.executeTmuxFanout(context.Background(), step, targets, runner)
	if err != nil {
		t.Fatalf("executeTmuxFanout: %v", err)
	}

	// 50 per-host + 1 merged
	if len(results) != numTargets+1 {
		t.Errorf("want %d results, got %d", numTargets+1, len(results))
	}

	peak := atomic.LoadInt64(&maxConcurrent)
	if peak > 32 {
		t.Errorf("max concurrent %d exceeds semaphore bound of 32", peak)
	}
	if peak < 2 {
		t.Errorf("max concurrent %d — expected parallelism", peak)
	}
}

func TestFanout_PanicRecovery(t *testing.T) {
	store, _ := NewOutputStore(nil)
	ex := NewExecutor(store, nil)

	step := &Step{
		Name:   "test-panic",
		Config: StepConfig{Timeout: 10, ContinueOnError: true},
	}
	targets := []string{"h1:s1"}

	runner := func(ctx context.Context, target string) (StepResult, error) {
		panic("simulated bug")
	}

	results, err := ex.executeTmuxFanout(context.Background(), step, targets, runner)
	if err != nil {
		t.Fatalf("executeTmuxFanout should not return error on panic: %v", err)
	}

	found := false
	for _, r := range results {
		if r.Host == "h1:s1" {
			found = true
			if r.ExitCode != 1 {
				t.Errorf("panic result exit code: want 1, got %d", r.ExitCode)
			}
			if !strings.Contains(r.Error, "panic:") {
				t.Errorf("panic result error should contain 'panic:', got %q", r.Error)
			}
		}
	}
	if !found {
		t.Error("h1:s1 result missing after panic")
	}
}
