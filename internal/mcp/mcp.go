package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func createMCPServer(store *inventory.InventoryStore) *server.MCPServer {
	s := server.NewMCPServer(
		"shellkit",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	tool := mcp.NewTool("ssh",
		mcp.WithDescription(`SSH to remote hosts via a step-based DSL. Brief by design — load the "shellkit-expert" skill for the full reference (actions, tmux verbs, host resolution, address routing, pipelines).

FORMAT — "input" is one or more blocks:
  ### step-name
  {json config}        optional; its keys pick the action
                       (blank line)
  script body
Steps run sequentially; later steps read earlier steps' output.

ACTIONS (chosen by config key):
  {"ssh": "host"}            run body on a remote host — name resolves via
                             inventory, ~/.ssh/config alias, or raw user@host[:port]
  {"ssh": ["h1","h2"]}      fan out the same body across hosts in parallel
  {"list": true}             list inventory hosts ("filter": "k=v" to narrow)
  {"tmux": "host:session"}   drive an interactive remote tmux session via verbs
  body only, no config       run locally (post-process output, scp/rsync)
  ### help                   print advanced pipeline + tmux verb examples

CONFIG FIELDS:
  timeout            seconds before kill (default 360)
  entrypoint         interpreter (default bash): bash sh zsh python3 python
                     node deno bun ruby perl
  trace              bash only, on by default — command trace shown on timeout
  continue_on_error  proceed to the next step even if this one fails
  jump               SSH ProxyJump host — hop through a bastion to reach the target

OUTPUT & CHAINING (GitHub-Actions style):
  echo "key=value" >> $OUTPUT          export a value from a step
  {{step.outputs.key}}                 read it in a later step
  {{step.output}}                      path to a step's full stdout file
  {{host.wan_ip}}                      inventory address — fields: wan_ip,
                                       lan_ip, wireguard_ip, tailscale_ip,
                                       easytier_ip, user, port (there is no "ip")
  Substitution is LITERAL (not shell-escaped) — quote values yourself.

Send "### help" for multi-step pipelines, cross-host coordination, non-bash
entrypoints, and the full tmux verb reference.`),
		mcp.WithString("input",
			mcp.Required(),
			mcp.Description("One or more DSL blocks. Format: ### name, optional {json config}, blank line, script body. Steps execute in order with template references resolved from prior results."),
		),
	)

	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		input, ok := req.GetArguments()["input"].(string)
		if !ok || input == "" {
			return mcp.NewToolResultError("input is required"), nil
		}

		// Extract session ID from context if available
		sessionID := clientSessionFromContext(ctx)

		// Allocate the call-id up front so live events share the same identifier.
		callID := shortID()

		// Best-effort: open the live event stream. If it fails, the call
		// still runs — only live observability is degraded.
		stream, esErr := NewEventStream(callID)
		if esErr != nil {
			log.Printf("event stream open failed: %v", esErr)
		}
		ctx = ContextWithEventStream(ctx, stream)

		// Wire MCP server for live log notifications (best-effort).
		if mcpSrv := server.ServerFromContext(ctx); mcpSrv != nil {
			stream.SetMCP(mcpSrv, ctx)
		}

		emitCallEnd := func(status, errMsg string) {
			if stream == nil {
				return
			}
			fields := map[string]any{
				"status":      status,
				"duration_ms": time.Since(start).Milliseconds(),
			}
			if errMsg != "" {
				fields["error"] = errMsg
			}
			stream.Emit("call-end", fields)
			stream.Close()
		}

		steps, err := ParseDSL(input)
		if err != nil {
			emitCallEnd("error", err.Error())
			return mcp.NewToolResultError(fmt.Sprintf("parse error: %v", err)), nil
		}

		// Now that steps are parsed, emit call-start with the structural
		// summary so the dashboard can render the step plan immediately.
		if stream != nil {
			stream.Emit("call-start", map[string]any{
				"session_id": sessionID,
				"input":      input,
				"steps":      BriefFromSteps(steps),
				"pid":        os.Getpid(),
			})
		}

		// Progress: emit periodic MCP notifications with live output
		// summary (step name, executing command, last N stdout lines).
		// 3s interval keeps output feeling live for long-running commands.
		keepaliveDone := make(chan struct{})
		if mcpSrv := server.ServerFromContext(ctx); mcpSrv != nil {
			go func() {
				ticker := time.NewTicker(3 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						elapsed := int(time.Since(start).Seconds())
						_ = mcpSrv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
							"progressToken": callID,
							"progress":      elapsed,
							"message":       stream.ProgressSummary(elapsed),
						})
					case <-keepaliveDone:
						return
					case <-ctx.Done():
						return
					}
				}
			}()
		}

		// Read live inventory on each request
		servers := store.Get()

		outStore, err := NewOutputStore(servers)
		if err != nil {
			close(keepaliveDone)
			emitCallEnd("error", err.Error())
			return mcp.NewToolResultError(fmt.Sprintf("init error: %v", err)), nil
		}

		executor := NewExecutor(outStore, servers)
		results, err := executor.Execute(ctx, steps)
		close(keepaliveDone)

		// Format result text before cleanup removes temp files.
		var resultText string
		if err != nil {
			emitCallEnd("error", err.Error())
			outStore.Cleanup()
			return mcp.NewToolResultError(fmt.Sprintf("execution error: %v", err)), nil
		}

		resultText = formatResults(results, outStore)
		emitCallEnd("ok", "")
		outStore.Cleanup()
		return mcp.NewToolResultText(resultText), nil
	})

	return s
}

func RunMCP(store *inventory.InventoryStore) error {
	return server.ServeStdio(createMCPServer(store))
}

func bearerAuthMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="shellkit"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type clientSessionKey struct{}

func clientSessionFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientSessionKey{}).(string); ok {
		return v
	}
	return ""
}

type clientCwdKey struct{}

func clientCwdFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientCwdKey{}).(string); ok {
		return v
	}
	return ""
}

func RunMCPHTTP(store *inventory.InventoryStore, port int) error {
	token := mcpToken()
	if token == "" {
		return fmt.Errorf("no auth token — run 'shellkit mcp start' or set SHELLKIT_MCP_TOKEN")
	}

	s := createMCPServer(store)
	mcpHandler := server.NewStreamableHTTPServer(s,
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if cwd := r.Header.Get("X-Client-Cwd"); cwd != "" {
				ctx = context.WithValue(ctx, clientCwdKey{}, cwd)
			}
			if sid := r.Header.Get("X-Session-Id"); sid != "" {
				ctx = context.WithValue(ctx, clientSessionKey{}, sid)
			}
			return ctx
		}),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	handler := bearerAuthMiddleware(token, mux)

	writePid(os.Getpid())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		os.Remove(mcpPidFile())
		os.Exit(0)
	}()
	defer os.Remove(mcpPidFile())

	addr := fmt.Sprintf(":%d", port)
	fmt.Fprintf(os.Stderr, "shellkit mcp http://localhost:%d/mcp (pid %d) [auth: bearer]\n", port, os.Getpid())
	return http.ListenAndServe(addr, handler)
}

func formatResults(results []StepResult, store *OutputStore) string {
	var b strings.Builder

	for _, r := range results {
		host := r.Host
		if host == "" {
			host = "local"
		}

		fmt.Fprintf(&b, "=== %s [%s] exit:%d ===\n", r.Name, host, r.ExitCode)

		if r.TimedOut {
			if r.TimeoutSec > 0 {
				fmt.Fprintf(&b, "TIMED OUT (after %ds)\n", r.TimeoutSec)
			} else {
				fmt.Fprintf(&b, "TIMED OUT\n")
			}
		}

		if r.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", r.Error)
		}

		if r.FilePath != "" {
			lines := strings.Count(r.Stdout, "\n") + 1
			if r.Stdout == "" {
				lines = 0
			}
			fmt.Fprintf(&b, "output: %s (%d lines)\n", r.FilePath, lines)
		}

		if len(r.Outputs) > 0 {
			for k, v := range r.Outputs {
				fmt.Fprintf(&b, "  %s=%s\n", k, v)
			}
		}

		if r.Stderr != "" {
			fmt.Fprintf(&b, "stderr: %s\n", mcpTruncate(r.Stderr, 4000))
		}

		if len(r.Trace) > 0 && (r.TimedOut || r.ShowTrace) {
			if r.TimedOut {
				fmt.Fprintf(&b, "command trace (timed out):\n")
			} else {
				fmt.Fprintf(&b, "command trace:\n")
			}
			showTrace := r.Trace
			cap := 20
			if r.ShowTrace && !r.TimedOut {
				cap = 50 // show more when explicitly requested
			}
			if len(showTrace) > cap {
				fmt.Fprintf(&b, "  ... (%d earlier commands omitted)\n", len(showTrace)-cap)
				showTrace = showTrace[len(showTrace)-cap:]
			}
			for i, t := range showTrace {
				dur := ""
				if i < len(showTrace)-1 {
					d := showTrace[i+1].ElapsedSec - t.ElapsedSec
					if d > 0 {
						dur = fmt.Sprintf(" (%ds)", d)
					}
				} else if r.TimedOut {
					dur = "  ← timed out here"
				}
				fmt.Fprintf(&b, "  +%ds  %s%s\n", t.ElapsedSec, mcpTruncate(t.Command, 500), dur)
			}
		}

		preview := mcpTruncate(r.Stdout, 16000)
		if preview != "" {
			fmt.Fprintf(&b, "preview:\n%s\n", indent(preview, "  "))
		}

		b.WriteString("\n")
	}

	return b.String()
}

func mcpTruncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "\n... (truncated)"
}

func shortID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func indent(s string, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
