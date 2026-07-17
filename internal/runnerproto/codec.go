package runnerproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrProtocol is the sentinel wrapped by every decode failure that is a wire
// contract violation — invalid JSON, an unknown frame type, a mismatched
// payload, or a line over [MaxLineBytes]. Callers use errors.Is(err,
// ErrProtocol) to distinguish a protocol error (kill the connection, fall back)
// from io.EOF (clean end of stream).
var ErrProtocol = errors.New("runnerproto: protocol error")

// flusher is satisfied by buffered writers (e.g. *bufio.Writer) so [Encoder]
// can push each frame out immediately when the caller wraps a buffer.
type flusher interface{ Flush() error }

// Encoder writes frames as ndjson (one JSON object per line) to an io.Writer.
// Its Encode method is safe for concurrent use: the runner emits trace and io
// frames from multiple goroutines onto a single stdout, and the mutex keeps
// their lines from interleaving.
type Encoder struct {
	mu sync.Mutex
	w  io.Writer
}

// NewEncoder returns an [Encoder] writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Encode validates f, then writes it as a single newline-terminated JSON line in
// one Write call, flushing w if it is buffered. It does not buffer internally,
// so a frame is on the wire the moment Encode returns — trace events arrive live.
func (e *Encoder) Encode(f Frame) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("runnerproto: encode: %w", err)
	}
	data, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("runnerproto: marshal %s frame: %w", f.Type, err)
	}
	// encoding/json escapes control characters, so data never contains a raw
	// newline; a single trailing '\n' is a safe frame delimiter.
	data = append(data, '\n')

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.w.Write(data); err != nil {
		return fmt.Errorf("runnerproto: write %s frame: %w", f.Type, err)
	}
	if fl, ok := e.w.(flusher); ok {
		if err := fl.Flush(); err != nil {
			return fmt.Errorf("runnerproto: flush %s frame: %w", f.Type, err)
		}
	}
	return nil
}

// Decoder reads ndjson frames from an io.Reader. It caps each line at
// [MaxLineBytes] instead of the default bufio.Scanner 64KB token size, so an
// oversized line surfaces as an [ErrProtocol] rather than a silent truncation.
// A Decoder is not safe for concurrent use.
type Decoder struct {
	sc *bufio.Scanner
}

// NewDecoder returns a [Decoder] reading from r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	return &Decoder{sc: sc}
}

// Decode reads and returns the next frame. It is strict: a non-empty line that
// is not a valid frame yields an [ErrProtocol]; a clean end of stream yields
// io.EOF. Blank lines are skipped. Use [Decoder.DecodeHello] for the handshake
// read, which additionally tolerates leading non-frame noise.
func (d *Decoder) Decode() (Frame, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return Frame{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return parseFrame(line)
	}
}

// DecodeHello reads the handshake hello, skipping any leading non-frame noise
// (login banners, MOTD, blank lines) before the first parseable frame — a
// defensive guard against bootstrap junk on the stream. The first valid frame
// MUST be a hello; anything else is an [ErrProtocol]. A single line over
// [MaxLineBytes] is a protocol error even here. After DecodeHello returns, use
// [Decoder.Decode]; noise is no longer tolerated.
func (d *Decoder) DecodeHello() (HelloFrame, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return HelloFrame{}, fmt.Errorf("%w: stream closed before hello frame", ErrProtocol)
			}
			return HelloFrame{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue // blank noise
		}
		f, perr := parseFrame(line)
		if perr != nil {
			continue // non-frame noise (banner/MOTD) — skip until the first real frame
		}
		if f.Type != FrameHello {
			return HelloFrame{}, fmt.Errorf("%w: expected hello frame, got %q", ErrProtocol, f.Type)
		}
		return *f.Hello, nil
	}
}

// readLine returns the next line's bytes (newline stripped). The returned slice
// is only valid until the next read. It maps an oversized line to an
// [ErrProtocol] and a clean end of stream to io.EOF.
func (d *Decoder) readLine() ([]byte, error) {
	if d.sc.Scan() {
		return d.sc.Bytes(), nil
	}
	if err := d.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("%w: line exceeds %d bytes", ErrProtocol, MaxLineBytes)
		}
		return nil, err
	}
	return nil, io.EOF
}

// parseFrame unmarshals one line into a validated [Frame]. Invalid JSON, an
// unknown type, or a mismatched payload becomes an [ErrProtocol].
func parseFrame(line []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("%w: invalid JSON: %v", ErrProtocol, err)
	}
	if err := f.Validate(); err != nil {
		return Frame{}, fmt.Errorf("%w: %v", ErrProtocol, err)
	}
	return f, nil
}
