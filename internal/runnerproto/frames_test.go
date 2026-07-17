package runnerproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// roundTrip encodes one frame, decodes it back with a strict decoder, and
// returns the decoded frame — the core golden assertion for the frame set.
func roundTrip(t *testing.T, f Frame) Frame {
	t.Helper()
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(f); err != nil {
		t.Fatalf("encode %s: %v", f.Type, err)
	}
	if bytes.Count(buf.Bytes(), []byte{'\n'}) != 1 {
		t.Fatalf("%s frame: expected exactly one newline-delimited line, got %q", f.Type, buf.String())
	}
	got, err := NewDecoder(&buf).Decode()
	if err != nil {
		t.Fatalf("decode %s: %v", f.Type, err)
	}
	return got
}

func TestRoundTripAllFrames(t *testing.T) {
	cases := []struct {
		name  string
		frame Frame
	}{
		{"hello-daemon", Frame{Type: FrameHello, Hello: &HelloFrame{Proto: ProtoVersion, Role: RoleDaemon}}},
		{"hello-runner-ack", Frame{Type: FrameHello, Hello: &HelloFrame{Proto: ProtoVersion, Role: RoleRunner, OS: "linux", Arch: "amd64", Version: "abc123"}}},
		{"run", Frame{Type: FrameRun, Run: &RunFrame{
			Program:   []byte("echo hi\ntrap cleanup EXIT\n"),
			Env:       map[string]string{"OUTPUT": "/tmp/out"},
			TimeoutNS: 300_000_000_000,
		}}},
		{"run-entrypoint", Frame{Type: FrameRun, Run: &RunFrame{
			Program:    []byte("print('hi')\n"),
			Entrypoint: "python3",
		}}},
		{"file", Frame{Type: FrameFile, File: &FileFrame{Name: "prev.out", Data: []byte("staged contents\n")}}},
		{"file-binary", Frame{Type: FrameFile, File: &FileFrame{Name: "blob.bin", Data: []byte{0x00, 0xff, 0xfe, 0x80, 0x7f}}}},
		{"trace-start", Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdStart, Seq: 1, Argv: []string{"ls", "-la", "/tmp"}}}},
		{"trace-end", Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdEnd, Seq: 1, Exit: 0, DurNS: 4_200_000}}},
		{"trace-end-nonzero", Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdEnd, Seq: 2, Exit: 137, DurNS: 5_000_000_000}}},
		{"io-text", Frame{Type: FrameIO, IO: &IOFrame{FD: 1, Data: "hello world\n"}}},
		{"output", Frame{Type: FrameOutput, Output: &OutputFrame{Values: map[string]string{"ip": "10.0.0.1", "port": "22"}}}},
		{"signal", Frame{Type: FrameSignal, Signal: &SignalFrame{Signal: SignalTERM}}},
		{"result", Frame{Type: FrameResult, Result: &ResultFrame{Exit: 0, WallNS: 1_500_000_000}}},
		{"result-error", Frame{Type: FrameResult, Result: &ResultFrame{Exit: 2, WallNS: 10_000, Error: "interp panic recovered"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.frame)
			if !reflect.DeepEqual(got, tc.frame) {
				t.Fatalf("round-trip mismatch:\n want %+v\n  got %+v", tc.frame, got)
			}
		})
	}
}

// TestLargeOutputFrame proves an output value well past the 64KB io-chunk bound
// round-trips whole — output frames are not chunked, so the codec must carry a
// large single line without the default-Scanner 64KB truncation trap.
func TestLargeOutputFrame(t *testing.T) {
	const size = 200 * 1024 // 200 KiB, > MaxIOChunkBytes, < MaxLineBytes
	big := strings.Repeat("shellkit-", size/9+1)[:size]
	f := Frame{Type: FrameOutput, Output: &OutputFrame{Values: map[string]string{"blob": big}}}

	got := roundTrip(t, f)
	if !reflect.DeepEqual(got, f) {
		t.Fatal("large output frame did not round-trip intact")
	}
	if l := len(got.Output.Values["blob"]); l != size {
		t.Fatalf("output value truncated: want %d bytes, got %d", size, l)
	}
}

// TestMultiChunkIOStream proves a large output stream chunked by SplitIO carries
// through the codec with no truncation: reassembling the decoded chunks equals
// the original bytes, and no chunk exceeds MaxIOChunkBytes.
func TestMultiChunkIOStream(t *testing.T) {
	orig := bytes.Repeat([]byte("The quick brown fox 0123456789\n"), 10_000) // ~300 KiB UTF-8
	frames := SplitIO(1, orig)
	if len(frames) < 4 {
		t.Fatalf("expected the stream to span several chunks, got %d", len(frames))
	}

	var wire bytes.Buffer
	enc := NewEncoder(&wire)
	for _, io := range frames {
		if len(io.Data) > 0 && !io.B64 && len(io.Data) > MaxIOChunkBytes {
			t.Fatalf("raw chunk exceeds MaxIOChunkBytes: %d", len(io.Data))
		}
		if err := enc.Encode(Frame{Type: FrameIO, IO: &io}); err != nil {
			t.Fatalf("encode io chunk: %v", err)
		}
	}

	dec := NewDecoder(&wire)
	var got bytes.Buffer
	for {
		f, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode io chunk: %v", err)
		}
		payload, err := f.IO.Bytes()
		if err != nil {
			t.Fatalf("decode chunk payload: %v", err)
		}
		got.Write(payload)
	}
	if !bytes.Equal(got.Bytes(), orig) {
		t.Fatalf("reassembled stream differs: want %d bytes, got %d", len(orig), got.Len())
	}
}

// TestBinaryIOChunkB64 proves a non-UTF-8 chunk is flagged b64 and round-trips
// byte-for-byte, while a valid UTF-8 chunk stays raw.
func TestBinaryIOChunkB64(t *testing.T) {
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x80, 0xc3, 0x28} // 0xc3 0x28 is invalid UTF-8
	bf := EncodeIOChunk(2, binary)
	if !bf.B64 {
		t.Fatal("binary chunk must set B64")
	}
	got := roundTrip(t, Frame{Type: FrameIO, IO: &bf})
	raw, err := got.IO.Bytes()
	if err != nil {
		t.Fatalf("decode binary payload: %v", err)
	}
	if !bytes.Equal(raw, binary) {
		t.Fatalf("binary chunk corrupted: want %x, got %x", binary, raw)
	}

	text := []byte("plain ascii and üñîçödé")
	tf := EncodeIOChunk(1, text)
	if tf.B64 {
		t.Fatal("valid UTF-8 chunk must not set B64")
	}
	if tf.Data != string(text) {
		t.Fatal("UTF-8 chunk must carry raw text")
	}
}

// TestHelloSkipsNoiseLines proves the handshake read tolerates leading banner /
// MOTD / blank / unknown-type noise, then hands a clean strict stream afterward.
func TestHelloSkipsNoiseLines(t *testing.T) {
	var wire bytes.Buffer
	wire.WriteString("Welcome to Ubuntu 24.04 LTS\n")
	wire.WriteString("\n")
	wire.WriteString("Last login: Tue Jul 16\n")
	wire.WriteString(`{"garbage":true}` + "\n") // JSON but not a valid frame
	enc := NewEncoder(&wire)
	if err := enc.Encode(Frame{Type: FrameHello, Hello: &HelloFrame{Proto: ProtoVersion, Role: RoleRunner, OS: "linux", Arch: "amd64"}}); err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := enc.Encode(Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdStart, Seq: 1, Argv: []string{"true"}}}); err != nil {
		t.Fatalf("encode trace: %v", err)
	}

	dec := NewDecoder(&wire)
	hello, err := dec.DecodeHello()
	if err != nil {
		t.Fatalf("DecodeHello over noise: %v", err)
	}
	if hello.Role != RoleRunner || hello.OS != "linux" {
		t.Fatalf("hello fields wrong after skipping noise: %+v", hello)
	}
	// After the handshake the stream is strict and the next real frame decodes.
	next, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode after hello: %v", err)
	}
	if next.Type != FrameTrace || next.Trace.Seq != 1 {
		t.Fatalf("post-hello frame wrong: %+v", next)
	}
}

// TestHelloNonHelloFirstFrameErrors proves that if the first parseable frame is
// not a hello, the handshake read is a protocol error (not silently accepted).
func TestHelloNonHelloFirstFrameErrors(t *testing.T) {
	var wire bytes.Buffer
	if err := NewEncoder(&wire).Encode(Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdStart, Seq: 1}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	_, err := NewDecoder(&wire).DecodeHello()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol for non-hello first frame, got %v", err)
	}
}

// TestHelloEOFBeforeFrame proves an all-noise stream that never yields a frame
// is a protocol error, not a silent empty hello.
func TestHelloEOFBeforeFrame(t *testing.T) {
	wire := strings.NewReader("banner line only\nno frames here\n")
	_, err := NewDecoder(wire).DecodeHello()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol on EOF before hello, got %v", err)
	}
}

// TestOversizedLineProtocolError proves a single line over MaxLineBytes surfaces
// as a protocol error rather than a silent truncation (the default-Scanner
// 64KB ErrTooLong trap this codec exists to avoid).
func TestOversizedLineProtocolError(t *testing.T) {
	// A ~200KB line decodes fine...
	ok := `{"type":"io","io":{"fd":1,"data":"` + strings.Repeat("x", 200*1024) + `"}}` + "\n"
	if _, err := NewDecoder(strings.NewReader(ok)).Decode(); err != nil {
		t.Fatalf("200KB line should decode, got %v", err)
	}
	// ...a 1.5MB line does not, and the error is a loud protocol error.
	huge := "{" + strings.Repeat("x", 3*MaxLineBytes/2) + "\n"
	_, err := NewDecoder(strings.NewReader(huge)).Decode()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("want ErrProtocol for oversized line, got %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", MaxLineBytes)) {
		t.Fatalf("oversized-line error should name the cap, got %v", err)
	}
}

// TestStrictDecodeRejectsGarbage proves that after the handshake, an unparseable
// line is a protocol error, not skipped.
func TestStrictDecodeRejectsGarbage(t *testing.T) {
	for _, line := range []string{
		"this is not json\n",
		`{"type":"nope"}` + "\n",                      // unknown type
		`{"type":"trace","result":{"exit":0}}` + "\n", // payload mismatches type
		`{"type":"hello"}` + "\n",                     // type set, no payload
		`{"type":"hello","hello":{},"run":{}}` + "\n", // two payloads
	} {
		_, err := NewDecoder(strings.NewReader(line)).Decode()
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("line %q: want ErrProtocol, got %v", line, err)
		}
	}
}

// TestEncodeValidatesEnvelope proves Encode refuses a malformed envelope so a
// mismatched type/payload never reaches the wire.
func TestEncodeValidatesEnvelope(t *testing.T) {
	bad := []Frame{
		{Type: FrameHello},                                       // no payload
		{Type: FrameHello, Run: &RunFrame{}},                     // payload mismatches type
		{Type: FrameTrace, Trace: &TraceFrame{}, IO: &IOFrame{}}, // two payloads
		{Type: "bogus", Hello: &HelloFrame{}},                    // unknown type
	}
	for i, f := range bad {
		if err := NewEncoder(io.Discard).Encode(f); err == nil {
			t.Fatalf("case %d: expected Encode to reject %+v", i, f)
		}
	}
}

// TestCleanEOF proves a fully-consumed stream reports io.EOF (not a protocol
// error) so a caller can tell a clean close from a wire violation.
func TestCleanEOF(t *testing.T) {
	var wire bytes.Buffer
	if err := NewEncoder(&wire).Encode(Frame{Type: FrameResult, Result: &ResultFrame{Exit: 0}}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec := NewDecoder(&wire)
	if _, err := dec.Decode(); err != nil {
		t.Fatalf("first decode: %v", err)
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF at clean end, got %v", err)
	}
}

// TestEncoderConcurrent proves Encode serializes concurrent writers so lines
// never interleave — the runner emits trace and io frames from many goroutines
// onto one stdout.
func TestEncoderConcurrent(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	const workers, per = 8, 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				f := Frame{Type: FrameTrace, Trace: &TraceFrame{Event: TraceCmdEnd, Seq: id*1000 + i, Exit: 0}}
				if err := enc.Encode(f); err != nil {
					t.Errorf("worker %d: encode: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	dec := NewDecoder(&buf)
	count := 0
	for {
		f, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode frame %d (line corruption from interleaving?): %v", count, err)
		}
		if f.Type != FrameTrace {
			t.Fatalf("frame %d: unexpected type %q", count, f.Type)
		}
		count++
	}
	if count != workers*per {
		t.Fatalf("want %d frames, got %d", workers*per, count)
	}
}
