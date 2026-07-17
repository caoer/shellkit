package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/caoer/shellkit/internal/runnerproto"
)

// encodeFrames serializes frames to an ndjson byte stream, used to drive the
// runner's stdin in loop tests.
func encodeFrames(t *testing.T, frames ...runnerproto.Frame) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := runnerproto.NewEncoder(&buf)
	for _, f := range frames {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encode %s frame: %v", f.Type, err)
		}
	}
	return buf.Bytes()
}

// decodeFrames reads every frame from an ndjson stream until EOF.
func decodeFrames(t *testing.T, r *bytes.Buffer) []runnerproto.Frame {
	t.Helper()
	dec := runnerproto.NewDecoder(r)
	var out []runnerproto.Frame
	for {
		f, err := dec.Decode()
		if err != nil {
			break
		}
		out = append(out, f)
	}
	return out
}

// writeFileErr is the error-returning file writer the test helpers wrap.
func writeFileErr(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func daemonHello() runnerproto.Frame {
	return runnerproto.Frame{
		Type:  runnerproto.FrameHello,
		Hello: &runnerproto.HelloFrame{Proto: runnerproto.ProtoVersion, Role: runnerproto.RoleDaemon},
	}
}

// TestRun_Handshake asserts the runner acks a daemon hello with its own hello
// carrying the protocol version, runner role, and this platform.
func TestRun_Handshake(t *testing.T) {
	in := bytes.NewReader(encodeFrames(t, daemonHello()))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	frames := decodeFrames(t, &out)
	if len(frames) != 1 || frames[0].Type != runnerproto.FrameHello {
		t.Fatalf("expected one hello ack, got %+v", frames)
	}
	h := frames[0].Hello
	if h.Proto != runnerproto.ProtoVersion || h.Role != runnerproto.RoleRunner {
		t.Fatalf("bad hello ack: %+v", h)
	}
	if h.OS != runtime.GOOS || h.Arch != runtime.GOARCH {
		t.Fatalf("hello ack platform = %s/%s, want %s/%s", h.OS, h.Arch, runtime.GOOS, runtime.GOARCH)
	}
}

// TestRun_HandshakeToleratesNoise verifies the hello read skips leading login
// banner / MOTD noise before the first frame.
func TestRun_HandshakeToleratesNoise(t *testing.T) {
	var in bytes.Buffer
	in.WriteString("Welcome to NixOS!\nLast login: whenever\n")
	in.Write(encodeFrames(t, daemonHello()))
	var out, errOut bytes.Buffer
	if err := run(&in, &out, &errOut); err != nil {
		t.Fatalf("run with banner noise: %v", err)
	}
	if frames := decodeFrames(t, &out); len(frames) != 1 || frames[0].Type != runnerproto.FrameHello {
		t.Fatalf("expected hello ack past the banner, got %+v", frames)
	}
}

// TestRun_SimpleBody drives a full step through the loop: hello + a run frame
// whose body echoes to stdout and writes $OUTPUT. Expect hello ack, an io frame,
// an output frame, then a result frame with exit 0.
func TestRun_SimpleBody(t *testing.T) {
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run:  &runnerproto.RunFrame{Program: []byte("echo hello\nprintf 'greeting=hi\\n' > \"$OUTPUT\"\n")},
	}
	in := bytes.NewReader(encodeFrames(t, daemonHello(), runFrame))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}

	frames := decodeFrames(t, &out)
	if len(frames) == 0 || frames[0].Type != runnerproto.FrameHello {
		t.Fatalf("missing hello ack: %+v", frames)
	}

	var sawIO, sawResult bool
	var outputVals map[string]string
	var stdout strings.Builder
	for _, f := range frames[1:] {
		switch f.Type {
		case runnerproto.FrameIO:
			if f.IO.FD == 1 {
				b, _ := f.IO.Bytes()
				stdout.Write(b)
				sawIO = true
			}
		case runnerproto.FrameOutput:
			outputVals = f.Output.Values
		case runnerproto.FrameResult:
			sawResult = true
			if f.Result.Exit != 0 {
				t.Errorf("exit = %d, want 0", f.Result.Exit)
			}
			if f.Result.WallNS <= 0 {
				t.Errorf("wall_ns = %d, want > 0", f.Result.WallNS)
			}
			if f.Result.Error != "" {
				t.Errorf("unexpected result error: %q", f.Result.Error)
			}
		}
	}
	if !sawIO || !strings.Contains(stdout.String(), "hello") {
		t.Errorf("stdout io frame missing 'hello': %q", stdout.String())
	}
	if outputVals["greeting"] != "hi" {
		t.Errorf("output frame = %v, want greeting=hi", outputVals)
	}
	if !sawResult {
		t.Errorf("missing result frame")
	}
}

// TestRun_FileFrameStagedIntoScratch stages a file, then runs a body that reads
// it back BY ABSOLUTE PATH from the scratch dir named by $OUTPUT's parent —
// proving file frame + run share one scratch dir. After the CWD-parity fix (#4)
// the body no longer runs FROM scratch (it inherits the runner's login dir), so a
// staged file is addressed by its absolute path, not a bare relative name.
func TestRun_FileFrameStagedIntoScratch(t *testing.T) {
	fileFrame := runnerproto.Frame{
		Type: runnerproto.FrameFile,
		File: &runnerproto.FileFrame{Name: "staged.txt", Data: []byte("payload-123")},
	}
	// $OUTPUT is <scratch>/.shellkit-output, so $(dirname "$OUTPUT")/staged.txt is
	// the staged file's absolute path regardless of the body's cwd.
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run:  &runnerproto.RunFrame{Program: []byte(`cat "$(dirname "$OUTPUT")/staged.txt"`)},
	}
	in := bytes.NewReader(encodeFrames(t, daemonHello(), fileFrame, runFrame))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	var stdout strings.Builder
	for _, f := range decodeFrames(t, &out) {
		if f.Type == runnerproto.FrameIO && f.IO.FD == 1 {
			b, _ := f.IO.Bytes()
			stdout.Write(b)
		}
	}
	if !strings.Contains(stdout.String(), "payload-123") {
		t.Fatalf("staged file not readable by body via absolute path: stdout = %q", stdout.String())
	}
}

// TestRun_FileFrameTraversalRefused proves a traversal filename is refused: the
// runner logs to stderr and never writes outside scratch, and the connection
// keeps serving (the following run frame still produces a result).
func TestRun_FileFrameTraversalRefused(t *testing.T) {
	fileFrame := runnerproto.Frame{
		Type: runnerproto.FrameFile,
		File: &runnerproto.FileFrame{Name: "../escape.txt", Data: []byte("nope")},
	}
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run:  &runnerproto.RunFrame{Program: []byte("true")},
	}
	in := bytes.NewReader(encodeFrames(t, daemonHello(), fileFrame, runFrame))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(errOut.String(), "refused file") {
		t.Errorf("expected a refusal diagnostic on stderr, got %q", errOut.String())
	}
	var sawResult bool
	for _, f := range decodeFrames(t, &out) {
		if f.Type == runnerproto.FrameResult {
			sawResult = true
		}
	}
	if !sawResult {
		t.Errorf("runner did not keep serving after a refused file")
	}
}

// TestRun_NonBashEntrypoint runs a body through a non-bash entrypoint as a
// supervised subprocess (skipped if sh is unavailable). sh reads the body on
// stdin and writes $OUTPUT.
func TestRun_NonBashEntrypoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix sh entrypoint")
	}
	runFrame := runnerproto.Frame{
		Type: runnerproto.FrameRun,
		Run: &runnerproto.RunFrame{
			Entrypoint: "sh",
			Program:    []byte("echo from-sh\nprintf 'e=1\\n' > \"$OUTPUT\"\n"),
		},
	}
	in := bytes.NewReader(encodeFrames(t, daemonHello(), runFrame))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	var stdout strings.Builder
	var vals map[string]string
	var exit int
	sawResult := false
	for _, f := range decodeFrames(t, &out) {
		switch f.Type {
		case runnerproto.FrameIO:
			if f.IO.FD == 1 {
				b, _ := f.IO.Bytes()
				stdout.Write(b)
			}
		case runnerproto.FrameOutput:
			vals = f.Output.Values
		case runnerproto.FrameResult:
			sawResult = true
			exit = f.Result.Exit
		}
	}
	if !sawResult || exit != 0 {
		t.Fatalf("subprocess result missing/non-zero: sawResult=%v exit=%d", sawResult, exit)
	}
	if !strings.Contains(stdout.String(), "from-sh") {
		t.Errorf("subprocess stdout missing: %q", stdout.String())
	}
	if vals["e"] != "1" {
		t.Errorf("subprocess $OUTPUT not collected: %v", vals)
	}
}

// TestRun_CleanExitOnEOF asserts a stream that ends after the handshake exits
// cleanly (nil), the U4 watchdog seam notwithstanding.
func TestRun_CleanExitOnEOF(t *testing.T) {
	in := bytes.NewReader(encodeFrames(t, daemonHello()))
	var out, errOut bytes.Buffer
	if err := run(in, &out, &errOut); err != nil {
		t.Fatalf("clean EOF should return nil, got %v", err)
	}
}
