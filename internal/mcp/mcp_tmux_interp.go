package mcp

import (
	"fmt"
	"strings"
)

func GenerateInterp(session, nonce, verbDelimiter string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "set -euo pipefail\n")
	fmt.Fprintf(&b, "set +o functrace\n")
	fmt.Fprintf(&b, "trap - DEBUG\n")
	fmt.Fprintf(&b, "SESS=%s\n", shellEscape(session))
	fmt.Fprintf(&b, "POLL_S=0.2\n")
	fmt.Fprintf(&b, "OUTPUT_FILE=\"$OUTPUT\"\n")
	fmt.Fprintf(&b, "# nonce: %s\n", nonce)

	fmt.Fprintf(&b, "if ! echo x | base64 -w0 >/dev/null 2>&1; then\n")
	fmt.Fprintf(&b, "  echo 'FATAL: base64 -w0 not supported (GNU coreutils required)' >&2\n")
	fmt.Fprintf(&b, "  exit 2\n")
	fmt.Fprintf(&b, "fi\n")

	b.WriteString(interpBody)
	fmt.Fprintf(&b, "done <<'%s'\n", verbDelimiter)
	return b.String()
}

const interpBody = `
decode_b64() { printf '%s' "$1" | base64 -d; }

do_spawn() {
  local args_b64=$1
  mapfile -d '' -t cmd_args < <(decode_b64 "$args_b64")
  tmux new-session -d -A -s "$SESS" -- "${cmd_args[@]}"
  tmux set-option -t "=$SESS" remain-on-exit on >/dev/null
}

do_send() {
  local data_b64=$1
  decode_b64 "$data_b64" | tmux load-buffer -
  tmux paste-buffer -d -t "=$SESS"
}

do_key() {
  for k in "$@"; do
    tmux send-keys -t "=$SESS" -- "$k"
  done
}

do_snap() {
  local pos=$1 lines=${2:-200}
  local content
  content=$(tmux capture-pane -t "=$SESS" -p -J -S "-${lines}" | base64 -w0)
  printf 'snap.%s=%s\n' "$pos" "$content" >> "$OUTPUT_FILE"
}

do_expect() {
  local pos=$1 pattern=$2 timeout=${3:-30} soft=${4:-0}
  local deadline=$(( SECONDS + timeout ))
  while (( SECONDS < deadline )); do
    if tmux capture-pane -t "=$SESS" -p -J -S -200 | grep -qE -- "$pattern"; then
      if (( soft )); then
        printf 'match.%s=true\n' "$pos" >> "$OUTPUT_FILE"
      fi
      return 0
    fi
    sleep "$POLL_S"
  done
  if (( soft )); then
    printf 'match.%s=false\n' "$pos" >> "$OUTPUT_FILE"
    return 0
  fi
  local snap; snap=$(tmux capture-pane -t "=$SESS" -p -J -S -200 2>/dev/null | base64 -w0)
  printf 'expect_failed.%s=%s\n' "$pos" "$snap" >> "$OUTPUT_FILE"
  printf 'expect_failed.%s.pattern=%s\n' "$pos" "$pattern" >> "$OUTPUT_FILE"
  return 1
}

do_kill() {
  tmux kill-session -t "=$SESS" 2>/dev/null || true
}

trap 'do_kill' EXIT

pos=0
while IFS= read -r line; do
  [[ -z $line ]] && continue
  verb=${line%% *}
  rest=${line#"$verb"}; rest=${rest# }
  case "$verb" in
    spawn_b64) do_spawn "$rest" ;;
    send_b64)  do_send "$rest" ;;
    key)       do_key $rest ;;
    snap)      lines=${rest##*lines=}
               [[ $lines == "$rest" ]] && lines=""
               do_snap "$pos" "${lines:-200}" ;;
    expect)    pat_b64=${rest%% *}; tmo=${rest##* }
               pat=$(decode_b64 "$pat_b64")
               do_expect "$pos" "$pat" "$tmo" 0 ;;
    expect_q)  pat_b64=${rest%% *}; tmo=${rest##* }
               pat=$(decode_b64 "$pat_b64")
               do_expect "$pos" "$pat" "$tmo" 1 ;;
    kill)      do_kill ;;
    sleep)     sleep "$rest" ;;
    *)         echo "unknown verb: $verb" >&2; exit 2 ;;
  esac
  pos=$((pos+1))
`
