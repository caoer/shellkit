package interp

import (
	"errors"
	"strings"
	"testing"
)

func TestPreflightParseErrorRefuses(t *testing.T) {
	// An unterminated construct must be refused BEFORE any ssh, with a position.
	v, err := Preflight([]byte("echo \"unterminated\ncat foo\n"))
	if err == nil {
		t.Fatalf("expected a parse error, got verdict %+v", v)
	}
	var pe *PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PreflightError, got %T: %v", err, err)
	}
	if pe.Line == 0 {
		t.Errorf("parse error carries no line position: %v", pe)
	}
	if !strings.Contains(pe.Error(), "syntax error") {
		t.Errorf("teaching error should read as a syntax error: %q", pe.Error())
	}
	if v.Route != "" {
		t.Errorf("a refused body must not carry a route, got %q", v.Route)
	}
}

func TestPreflightLangErrorRoutesRealbash(t *testing.T) {
	// zsh-only floating-point arithmetic is valid shell, wrong dialect: it should
	// fall back to real bash rather than be refused.
	v, err := Preflight([]byte("echo $(( 1.5 + 2 ))\n"))
	if err != nil {
		t.Fatalf("LangError body must not be a hard refusal: %v", err)
	}
	if v.Route != RouteRealbash {
		t.Fatalf("dialect feature should route to realbash, got %q", v.Route)
	}
	if !strings.Contains(v.Reason, "dialect") {
		t.Errorf("reason should name the dialect fallback: %q", v.Reason)
	}
}

// routeCase drives the table below.
type routeCase struct {
	name      string
	body      string
	wantRoute Route
	reasonHas string // substring the reason must contain (route==realbash only)
}

func TestRouting(t *testing.T) {
	cases := []routeCase{
		// ---- Class A: loud gaps -> realbash ----
		{"trap_named_signal", "trap 'x' INT TERM\n", RouteRealbash, "signal INT"},
		{"trap_lowercase_exit", "trap 'x' exit\n", RouteRealbash, "signal"},
		{"trap_numeric", "trap 'x' 0\n", RouteRealbash, "signal"},
		{"trap_list_flag", "trap -p\n", RouteRealbash, "trap -p"},
		{"kill", "kill -9 12\n", RouteRealbash, "kill"},
		{"ulimit", "ulimit -n\n", RouteRealbash, "ulimit"},
		{"jobs", "jobs\n", RouteRealbash, "jobs"},
		{"fg", "fg %1\n", RouteRealbash, "fg"},
		{"disown", "disown\n", RouteRealbash, "disown"},
		{"command_declare", "command declare x=1\n", RouteRealbash, "command declare"},
		{"command_local", "command local y=2\n", RouteRealbash, "command local"},
		{"printf_q", "printf '%q\\n' x\n", RouteRealbash, "%q"},
		{"printf_time", "printf '%(%s)T\\n' -1\n", RouteRealbash, "time"},
		{"printf_float", "printf '%f\\n' 1\n", RouteRealbash, "floating-point"},
		{"read_n", "read -n 3 v\n", RouteRealbash, "read -n"},
		{"read_timeout", "read -t 5 v\n", RouteRealbash, "read -t"},
		{"wait_n", "wait -n\n", RouteRealbash, "wait -n"},
		{"shopt_q", "shopt -q nullglob\n", RouteRealbash, "shopt -q"},
		{"shopt_unsupported_name", "shopt -s huponexit\n", RouteRealbash, "huponexit"},
		{"type_a", "type -a ls\n", RouteRealbash, "type -a"},
		{"select", "select x in a b; do break; done\n", RouteRealbash, "select"},
		{"mapfile_n", "mapfile -n 2 arr\n", RouteRealbash, "mapfile"},

		// ---- Class A: printf -v (assign) -> realbash ----
		{"printf_v", "printf -v out '%s' hi\n", RouteRealbash, "printf -v"},
		{"printf_v_combined", "printf -rv out '%s' hi\n", RouteRealbash, "printf -v"},

		// ---- Class B: silent-divergence idiom screen -> realbash ----
		{"pipe_final_cd", "echo x | cd /tmp\n", RouteRealbash, "final stage"},
		{"pipe_final_read", "echo x | read v\n", RouteRealbash, "final stage"},
		{"pipe_final_export", "echo x | export Y=1\n", RouteRealbash, "final stage"},
		{"pipe_final_while_read", "seq 3 | while read x; do :; done\n", RouteRealbash, "compound command"},
		{"pipe_final_for", "echo x | for i in a b; do :; done\n", RouteRealbash, "compound command"},
		{"pipe_final_subshell", "echo x | (read v)\n", RouteRealbash, "compound command"},
		{"pipe_final_block", "echo x | { read v; }\n", RouteRealbash, "compound command"},
		{"pipe_final_eval", "printf 'v\\n' | eval read y\n", RouteRealbash, "eval"},
		{"pipe_final_dynamic_head", "printf 'v\\n' | $cmd read y\n", RouteRealbash, "dynamic command head"},

		// ---- Class B: pipeline final-stage ALLOWLIST inversion -> realbash ----
		// The final stage stays on interp only if provably a plain external
		// command; every in-process construct below leaks parent state under
		// interp (bash subshell-isolates the final stage, discarding its effect).
		{"pipe_final_arith_incr", "seq 3 | ((n++))\n", RouteRealbash, "arithmetic command"},
		{"pipe_final_arith_plus", "seq 3 | (( n += 5 ))\n", RouteRealbash, "arithmetic command"},
		{"pipe_final_let", "seq 3 | let n=5\n", RouteRealbash, "`let`"},
		{"pipe_final_unset", "echo x | unset V\n", RouteRealbash, "unset"},
		{"pipe_final_mapfile", "printf 'a\\nb\\n' | mapfile -t A\n", RouteRealbash, "mapfile"},
		{"pipe_final_readarray", "printf 'a\\nb\\n' | readarray -t A\n", RouteRealbash, "readarray"},
		{"pipe_final_command_cd", "echo x | command cd sub\n", RouteRealbash, "command"},
		{"pipe_final_builtin_cd", "echo y | builtin cd sub\n", RouteRealbash, "builtin"},
		{"pipe_final_source", "echo y | source x.sh\n", RouteRealbash, "source"},
		{"pipe_final_dot", "echo y | . y.sh\n", RouteRealbash, "final stage"},
		{"pipe_final_bare_assign", "echo x | VAR=1\n", RouteRealbash, "bare assignment"},
		{"pipe_final_local_func", "fn() { cd /tmp; }\necho x | fn\n", RouteRealbash, "function `fn`"},

		// External-command final stage -> interp (must NOT over-screen).
		{"exec_redirect_no_cmd", "exec > log\n", RouteRealbash, "exec"},
		{"dollar_pid", "echo p-$$\n", RouteRealbash, "$$"},
		{"bang_pid", "sleep 1 & echo $!\n", RouteRealbash, "$!"},
		{"brace_dollar_pid", "echo \"${$}\"\n", RouteRealbash, "$$"},
		{"brace_bang_pid", "echo \"${!}\"\n", RouteRealbash, "$!"},
		{"trap_exit_plus_exec", "trap 'rm f' EXIT\nexec sleep 1\n", RouteRealbash, "EXIT"},
		{"ifs_plus_forloop", "IFS=,\nfor x in $d; do echo $x; done\n", RouteRealbash, "IFS"},

		// ---- Negative controls & clean bodies -> interp ----
		{"clean_echo", "echo hi\n", RouteInterp, ""},
		{"clean_export_keyword", "export FOO=bar\n", RouteInterp, ""},
		{"clean_declare_bare", "declare x=1\n", RouteInterp, ""},
		{"clean_pipe_plain", "echo x | grep x\n", RouteInterp, ""},
		// External-command final stages must stay on interp (allowlist must not
		// over-screen a plain forked command that cannot leak parent state).
		{"clean_pipe_awk", "cat f | awk '{print}'\n", RouteInterp, ""},
		{"clean_pipe_sed", "echo x | sed 's/x/y/'\n", RouteInterp, ""},
		{"clean_pipe_chain", "ls | sort | head\n", RouteInterp, ""},
		{"clean_pipe_wc", "seq 3 | wc -l\n", RouteInterp, ""},
		{"clean_trap_exit", "trap 'echo bye' EXIT\n", RouteInterp, ""},
		{"clean_trap_err", "trap 'echo e' ERR\n", RouteInterp, ""},
		{"clean_read_basic", "read -r -p 'p' v\n", RouteInterp, ""},
		{"clean_wait_plain", "wait\n", RouteInterp, ""},
		{"clean_type_plain", "type ls\n", RouteInterp, ""},
		{"clean_shopt_supported", "shopt -s globstar\n", RouteInterp, ""},
		{"clean_mapfile_t", "mapfile -t arr\n", RouteInterp, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := Preflight([]byte(c.body))
			if err != nil {
				t.Fatalf("unexpected preflight error: %v", err)
			}
			if v.Route != c.wantRoute {
				t.Fatalf("route = %q, want %q (reason: %s)", v.Route, c.wantRoute, v.Reason)
			}
			if v.Reason == "" {
				t.Errorf("verdict must always carry a reason")
			}
			if c.wantRoute == RouteRealbash && !strings.Contains(v.Reason, c.reasonHas) {
				t.Errorf("reason %q does not mention %q", v.Reason, c.reasonHas)
			}
		})
	}
}

// TestNoStaleFalsePositives locks the FIXED-at-v3.13.x forms to the interp route.
// Triggering on any of these would be a stale-knowledge bug (plan U1). Some of
// these diverge from bash at RUNTIME (declare -f formatting, ${a[@]@a}) but that
// is out of scope for the router — the requirement is only that they are not
// mistaken for gaps and are NOT sent to bash.
func TestNoStaleFalsePositives(t *testing.T) {
	fixed := []string{
		"declare -p X\n",
		"declare -f myfunc\n",
		"type -P ls\n",
		"type -p ls\n",
		"type -t ls\n",
		"read -a arr\n",
		"echo ${x@a}\n",
		"echo ${x@A}\n",
		"echo ${x@P}\n",
		"echo ${x@Q}\n",
		"declare -A m\n",
		// Indirect expansion / array-keys use Param.Value = the referenced NAME
		// (Excl=true), not "!" — they must not be mistaken for the ${!} pid form.
		"echo ${!ref}\n",
		"echo ${!arr[@]}\n",
		"echo ${#$}\n", // length of $$ (digit count) — not the colliding pid value
	}
	for _, body := range fixed {
		v, err := Preflight([]byte(body))
		if err != nil {
			t.Errorf("%q: unexpected error %v", body, err)
			continue
		}
		if v.Route != RouteInterp {
			t.Errorf("%q: routed to %q (stale-knowledge false-positive), want interp; reason=%s",
				strings.TrimSpace(body), v.Route, v.Reason)
		}
	}
}

// TestGapPriority checks that when several gaps match, the highest-priority
// (lowest number) class-A reason wins over a class-B idiom.
func TestGapPriority(t *testing.T) {
	// Contains both a class-A `kill` (prio 2) and a class-B `$$` (prio 22).
	v, err := Preflight([]byte("echo $$\nkill -9 $$\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Route != RouteRealbash {
		t.Fatalf("want realbash, got %q", v.Route)
	}
	if !strings.Contains(v.Reason, "kill") {
		t.Errorf("higher-priority `kill` reason should win, got %q", v.Reason)
	}
}
