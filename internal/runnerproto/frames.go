// Package runnerproto defines the wire protocol spoken between the shellkit
// daemon and the remote shellkit-runner over an ssh exec channel.
//
// # Transport and direction
//
// The daemon exec's the runner directly (no login shell), so both directions
// carry an ndjson stream: exactly one JSON object per line, each object a
// [Frame] envelope tagged by its "type" field. Three streams, three roles:
//
//   - stdin  (daemon → runner): hello, run, file, signal frames.
//   - stdout (runner → daemon): hello (as ack), trace, io, output, result frames.
//   - stderr (runner → daemon): free-form runner self-diagnostics, NEVER frames.
//
// The runner's protocol stdout (fd 1) is exclusively frames; a child process's
// stdout/stderr is re-framed into io frames by the runner and never inherited,
// so nothing a user command prints can be mistaken for a protocol frame.
//
// # Framing
//
// Each frame is a tagged union: the [Frame] envelope carries a [FrameType]
// discriminator plus exactly one non-nil payload pointer matching that type.
// The codec ([Encoder]/[Decoder], see codec.go) enforces the one-payload
// invariant on every read and write. The decoder never uses a default
// bufio.Scanner (its 64KB token cap silently truncates — a known Terraform /
// InfluxDB production bug); it caps lines at [MaxLineBytes] and surfaces an
// oversized line as a protocol error rather than truncating.
//
// # Conventions
//
//   - Protocol version is [ProtoVersion]; it rides in the hello handshake so
//     the two sides can detect a stale runner from day one.
//   - Every duration on the wire is an int64 count of nanoseconds
//     (RunFrame.TimeoutNS, TraceFrame.DurNS, ResultFrame.WallNS).
//   - io payloads are chunked to at most [MaxIOChunkBytes] raw bytes on the
//     producer side so no single line approaches [MaxLineBytes], and each chunk
//     is base64-encoded iff it is not valid UTF-8 (see [EncodeIOChunk]).
//   - Only the hello handshake read ([Decoder.DecodeHello]) tolerates leading
//     non-frame noise (login banners, MOTD); after the handshake an unparseable
//     line is a protocol error.
package runnerproto

import (
	"encoding/base64"
	"fmt"
	"unicode/utf8"
)

// ProtoVersion is the current wire-protocol version, advertised in the hello
// handshake (HelloFrame.Proto). Bump it on any breaking change to the frame set
// so a daemon speaking a newer protocol can detect and re-push a stale runner.
const ProtoVersion = 1

// MaxIOChunkBytes is the maximum number of raw (pre-base64) bytes an io-frame
// payload may carry. Producers MUST split larger output into multiple io frames
// (see [SplitIO]) so that no single ndjson line — even after base64 (4/3×) and
// JSON string escaping — approaches [MaxLineBytes].
const MaxIOChunkBytes = 64 << 10 // 64 KiB

// MaxLineBytes is the hard ceiling the decoder enforces on a single ndjson line.
// A line larger than this surfaces as a protocol error (never a silent
// truncation). Producers MUST keep every frame — including a large output
// frame — below this limit; $OUTPUT values are capped by the runner well under
// it to leave room for JSON escaping.
const MaxLineBytes = 1 << 20 // 1 MiB

// FrameType is the discriminator tag carried in every frame's "type" field.
type FrameType string

// The eight frame types exchanged over the exec channel.
const (
	// FrameHello is the version/proto/os-arch handshake. The daemon sends it
	// first on stdin; the runner echoes one back on stdout as the ack.
	FrameHello FrameType = "hello"
	// FrameRun carries a step to execute: program bytes, env, timeout.
	FrameRun FrameType = "run"
	// FrameFile stages a prior {{step.output}} into the runner's scratch dir.
	FrameFile FrameType = "file"
	// FrameTrace reports a per-command cmd_start / cmd_end event.
	FrameTrace FrameType = "trace"
	// FrameIO carries a chunk of a child's stdout or stderr.
	FrameIO FrameType = "io"
	// FrameOutput carries the collected $OUTPUT key=value pairs.
	FrameOutput FrameType = "output"
	// FrameSignal requests in-band cancellation (ssh signal delivery is
	// unreliable, so cancellation rides the wire instead).
	FrameSignal FrameType = "signal"
	// FrameResult closes a step: final exit code and wall-clock duration.
	FrameResult FrameType = "result"
)

// Hello handshake roles, carried in HelloFrame.Role so a receiver can tell the
// daemon's opening hello from the runner's ack.
const (
	// RoleDaemon marks the daemon's opening hello (sent on stdin).
	RoleDaemon = "daemon"
	// RoleRunner marks the runner's hello ack (sent on stdout); it reports the
	// runner's os/arch/version so the daemon can verify the bootstrapped binary.
	RoleRunner = "runner"
)

// Signal names carried in SignalFrame.Signal.
const (
	// SignalTERM requests a graceful terminate of the running step.
	SignalTERM = "TERM"
	// SignalKILL requests an immediate kill of the running step.
	SignalKILL = "KILL"
	// SignalINT requests an interrupt of the running step.
	SignalINT = "INT"
)

// TraceEvent distinguishes the two per-command trace phases.
type TraceEvent string

const (
	// TraceCmdStart is emitted just before an external command runs; it carries
	// the argv.
	TraceCmdStart TraceEvent = "cmd_start"
	// TraceCmdEnd is emitted after the command returns; it carries the exit code
	// and the monotonic duration in nanoseconds.
	TraceCmdEnd TraceEvent = "cmd_end"
)

// HelloFrame is the version / platform handshake exchanged in both directions.
type HelloFrame struct {
	// Proto is the wire-protocol version the sender speaks; see [ProtoVersion].
	Proto int `json:"proto"`
	// Role is [RoleDaemon] for the opening hello or [RoleRunner] for the ack.
	Role string `json:"role"`
	// OS is the runner's GOOS (runner ack only); empty in the daemon hello.
	OS string `json:"os,omitempty"`
	// Arch is the runner's GOARCH (runner ack only); empty in the daemon hello.
	Arch string `json:"arch,omitempty"`
	// Version is the runner binary's content-hash version (runner ack only) so
	// the daemon can detect a stale or mismatched binary.
	Version string `json:"version,omitempty"`
}

// RunFrame instructs the runner to execute one step.
type RunFrame struct {
	// Program is the step body, transmitted as exact bytes (JSON base64-encodes
	// it on the wire). It is never re-parsed/re-printed, so the runner executes
	// the verbatim body, preserving heredocs, quoting, and any non-UTF-8 bytes.
	Program []byte `json:"program"`
	// Env is the environment applied to the step. NOTE (plan decision #17): this
	// is a CLOSED allowlist — only {OUTPUT} — enforced by the runner in U3a, NOT
	// by this codec. The type stays a general map so the runner owns the policy;
	// it MUST NEVER be populated from os.Environ() or any password_ref / sops
	// material, which would spray decrypted secrets into every fleet runner.
	Env map[string]string `json:"env,omitempty"`
	// Entrypoint, when non-empty, names a non-bash interpreter (python3, node…)
	// the runner spawns as a supervised subprocess with whole-step timing.
	// Empty means the mvdan/sh interp engine runs the program.
	Entrypoint string `json:"entrypoint,omitempty"`
	// TimeoutNS is the wall-clock timeout in nanoseconds; 0 means no timeout.
	TimeoutNS int64 `json:"timeout_ns,omitempty"`
}

// FileFrame stages a file into the runner's scratch directory before the run,
// replacing the scp hop for cross-host {{step.output}} references.
type FileFrame struct {
	// Name is the target filename. The runner MUST reduce it to filepath.Base
	// and refuse traversal escapes (path-traversal guard lives in U3a).
	Name string `json:"name"`
	// Data is the file contents (JSON base64-encodes it on the wire, so binary
	// files stage exactly).
	Data []byte `json:"data,omitempty"`
}

// TraceFrame reports one per-command trace event. Argv is meaningful on a
// cmd_start; Exit and DurNS are meaningful on a cmd_end.
type TraceFrame struct {
	// Event is [TraceCmdStart] or [TraceCmdEnd].
	Event TraceEvent `json:"event"`
	// Seq is the monotonic per-step command sequence number.
	Seq int `json:"seq"`
	// Argv is the command's argument vector (cmd_start).
	Argv []string `json:"argv,omitempty"`
	// Line is the command's 1-based source line in the step body (cmd_start),
	// from the interp handler's position. 0 = unknown (older runner, or the
	// operation carried no position). Optional on the wire in both directions.
	Line int `json:"line,omitempty"`
	// Exit is the command's exit code (cmd_end).
	Exit int `json:"exit,omitempty"`
	// DurNS is the command's monotonic duration in nanoseconds (cmd_end).
	DurNS int64 `json:"dur_ns,omitempty"`
}

// IOFrame carries one chunk of a child process's stdout or stderr. Build one
// with [EncodeIOChunk] (or a stream with [SplitIO]) and read it back with
// [IOFrame.Bytes] so producer and consumer agree on the base64 rule.
type IOFrame struct {
	// FD is the source file descriptor: 1 for stdout, 2 for stderr.
	FD int `json:"fd"`
	// Data is the chunk payload: raw UTF-8 text when B64 is false, or the
	// base64 encoding of the raw bytes when B64 is true.
	Data string `json:"data"`
	// B64 is true iff Data is base64-encoded because the raw chunk is not valid
	// UTF-8 (binary output, or a multi-byte rune split across a chunk boundary).
	B64 bool `json:"b64,omitempty"`
}

// OutputFrame carries the $OUTPUT key=value pairs the runner collected.
type OutputFrame struct {
	// Values is the set of $OUTPUT keys to values.
	Values map[string]string `json:"values"`
}

// SignalFrame requests in-band cancellation of the running step. It rides the
// wire because ssh signal delivery is unreliable.
type SignalFrame struct {
	// Signal is [SignalTERM], [SignalKILL], or [SignalINT].
	Signal string `json:"signal"`
}

// ResultFrame closes a step.
type ResultFrame struct {
	// Exit is the step's final exit code.
	Exit int `json:"exit"`
	// WallNS is the step's total wall-clock duration in nanoseconds.
	WallNS int64 `json:"wall_ns"`
	// Error, when non-empty, is a runner-level failure message (e.g. a recovered
	// interp panic); empty on normal completion.
	Error string `json:"error,omitempty"`
}

// Frame is the tagged-union envelope for every message on the wire. Exactly one
// payload pointer is non-nil, and it matches Type; the codec enforces this on
// every encode and decode via [Frame.Validate].
type Frame struct {
	// Type is the discriminator naming which payload is set.
	Type FrameType `json:"type"`
	// Hello is set iff Type is [FrameHello].
	Hello *HelloFrame `json:"hello,omitempty"`
	// Run is set iff Type is [FrameRun].
	Run *RunFrame `json:"run,omitempty"`
	// File is set iff Type is [FrameFile].
	File *FileFrame `json:"file,omitempty"`
	// Trace is set iff Type is [FrameTrace].
	Trace *TraceFrame `json:"trace,omitempty"`
	// IO is set iff Type is [FrameIO].
	IO *IOFrame `json:"io,omitempty"`
	// Output is set iff Type is [FrameOutput].
	Output *OutputFrame `json:"output,omitempty"`
	// Signal is set iff Type is [FrameSignal].
	Signal *SignalFrame `json:"signal,omitempty"`
	// Result is set iff Type is [FrameResult].
	Result *ResultFrame `json:"result,omitempty"`
}

// Validate reports whether f is a well-formed envelope: Type is a known frame
// type, exactly one payload pointer is set, and it matches Type. The codec
// calls Validate on every encode and decode.
func (f Frame) Validate() error {
	present := make([]FrameType, 0, 1)
	if f.Hello != nil {
		present = append(present, FrameHello)
	}
	if f.Run != nil {
		present = append(present, FrameRun)
	}
	if f.File != nil {
		present = append(present, FrameFile)
	}
	if f.Trace != nil {
		present = append(present, FrameTrace)
	}
	if f.IO != nil {
		present = append(present, FrameIO)
	}
	if f.Output != nil {
		present = append(present, FrameOutput)
	}
	if f.Signal != nil {
		present = append(present, FrameSignal)
	}
	if f.Result != nil {
		present = append(present, FrameResult)
	}

	if !knownType(f.Type) {
		return fmt.Errorf("unknown frame type %q", f.Type)
	}
	if len(present) != 1 {
		return fmt.Errorf("frame type %q must carry exactly one payload, found %d", f.Type, len(present))
	}
	if present[0] != f.Type {
		return fmt.Errorf("frame type %q does not match payload %q", f.Type, present[0])
	}
	return nil
}

// knownType reports whether t is one of the eight defined frame types.
func knownType(t FrameType) bool {
	switch t {
	case FrameHello, FrameRun, FrameFile, FrameTrace, FrameIO, FrameOutput, FrameSignal, FrameResult:
		return true
	default:
		return false
	}
}

// EncodeIOChunk builds an [IOFrame] for a single chunk of a child's output on
// file descriptor fd, choosing base64 iff chunk is not valid UTF-8. The chunk
// SHOULD be at most [MaxIOChunkBytes] raw bytes; use [SplitIO] to chunk a larger
// buffer.
func EncodeIOChunk(fd int, chunk []byte) IOFrame {
	if utf8.Valid(chunk) {
		return IOFrame{FD: fd, Data: string(chunk), B64: false}
	}
	return IOFrame{FD: fd, Data: base64.StdEncoding.EncodeToString(chunk), B64: true}
}

// SplitIO chunks data into a sequence of [IOFrame]s for file descriptor fd,
// each carrying at most [MaxIOChunkBytes] raw bytes and its own base64 flag.
// Concatenating [IOFrame.Bytes] over the returned frames reconstructs data
// exactly, even when a fixed-size boundary splits a multi-byte rune (that chunk
// is simply flagged b64). It returns nil for empty data.
func SplitIO(fd int, data []byte) []IOFrame {
	if len(data) == 0 {
		return nil
	}
	var frames []IOFrame
	for len(data) > 0 {
		n := MaxIOChunkBytes
		if n > len(data) {
			n = len(data)
		}
		frames = append(frames, EncodeIOChunk(fd, data[:n]))
		data = data[n:]
	}
	return frames
}

// Bytes returns the raw payload bytes of the io frame, decoding base64 when B64
// is set. It errors only on a corrupt base64 payload.
func (f IOFrame) Bytes() ([]byte, error) {
	if f.B64 {
		b, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return nil, fmt.Errorf("runnerproto: decode io b64 payload: %w", err)
		}
		return b, nil
	}
	return []byte(f.Data), nil
}
