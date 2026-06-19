// synth-burst writes synthetic JSONL events at a fixed rate to the shellkit
// events directory. Used for stress-testing channel backpressure and the
// lag indicator.
//
// Usage:
//
//	go run testdata/synth-burst.go --rate 1000 --duration 30s --call-id smoke-test-XXX
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	rate := flag.Int("rate", 1000, "events per second")
	dur := flag.Duration("duration", 10*time.Second, "how long to emit stdout events")
	callID := flag.String("call-id", fmt.Sprintf("synth-%d", time.Now().UnixMilli()), "call ID for the event file")
	flag.Parse()

	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "state", "shellkit", "events")
	os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, *callID+".jsonl")

	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	emit := func(ev map[string]any) {
		ev["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
		data, _ := json.Marshal(ev)
		f.Write(append(data, '\n'))
	}

	steps := 1
	emit(map[string]any{
		"kind":  "call-start",
		"steps": []map[string]string{{"name": "burst", "action": "local"}},
	})
	emit(map[string]any{"kind": "step-start", "step": 0, "name": "burst"})

	interval := time.Second / time.Duration(*rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.After(*dur)
	n := 0
	fmt.Fprintf(os.Stderr, "emitting at %d ev/s for %s → %s\n", *rate, *dur, path)
loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			emit(map[string]any{"kind": "stdout", "step": 0, "line": fmt.Sprintf("burst-line-%d", n)})
			n++
		}
	}

	emit(map[string]any{"kind": "step-end", "step": 0, "exit_code": 0, "duration_ms": dur.Milliseconds()})
	emit(map[string]any{"kind": "call-end", "status": "ok"})

	fmt.Fprintf(os.Stderr, "done: %d stdout events in %d steps, call-id=%s\n", n, steps, *callID)
}
