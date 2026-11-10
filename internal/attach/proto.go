// Package attach implements the attach protocol between a session's monitor,
// which owns the agent's pty, and the terminals attached to it (design §8.2,
// docs/attach-protocol.md). The protocol is a stream of framed messages and
// does not depend on the transport; the Unix socket attach.sock is the only
// transport in use.
//
// The package holds both ends: Server, which the monitor runs, and Client,
// which `fastener attach` and the tests use.
package attach

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version this package speaks.
const Version = 1

// Frame types. Every frame is a 5-byte header (type, then the payload length
// as a big-endian uint32) followed by the payload.
const (
	TypeHello   byte = 1 // client → server, first frame: version u16, rows u16, cols u16
	TypeWelcome byte = 2 // server → client, answer to hello: version u16, rows u16, cols u16
	TypeData    byte = 3 // both ways: terminal bytes
	TypeResize  byte = 4 // client → server: rows u16, cols u16
	TypeDetach  byte = 5 // client → server: leaving; empty payload
	TypeExit    byte = 6 // server → client: the session ended; JSON ExitInfo
	TypeError   byte = 7 // server → client: JSON ErrorInfo; the server then closes
)

const headerLen = 5

// MaxPayload bounds every frame's payload. A larger length in a header is a
// protocol error: the receiver closes that connection.
const MaxPayload = 64 << 10

// Error codes carried in an error frame.
const (
	CodeVersion  = "version"  // the hello's version is not one the server speaks
	CodeProtocol = "protocol" // a malformed or unexpected frame
	CodeBusy     = "busy"     // too many clients are attached
	CodeSlow     = "slow"     // the client fell too far behind the output
)

// ErrorInfo is the payload of an error frame.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Versions lists the versions the server speaks (with CodeVersion).
	Versions []int `json:"versions,omitempty"`
}

// RemoteError is an error frame received from the server.
type RemoteError struct{ ErrorInfo }

func (e *RemoteError) Error() string {
	return fmt.Sprintf("attach: server refused (%s): %s", e.Code, e.Message)
}

// ExitInfo is the payload of an exit frame: how the session ended, as
// recorded in status.json.
type ExitInfo struct {
	EndedBy  string `json:"ended_by"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
	// AgentError is the parser's agent_error, so a client can map the
	// outcome exactly as `fastener wait` does.
	AgentError *bool `json:"agent_error,omitempty"`
}

// ErrFrameTooLarge is returned for a header whose length exceeds MaxPayload.
var ErrFrameTooLarge = errors.New("attach: frame payload too large")

// Frame is one decoded frame.
type Frame struct {
	Type    byte
	Payload []byte
}

// WriteFrame writes one frame. It refuses a payload over MaxPayload.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrFrameTooLarge
	}
	buf := make([]byte, headerLen+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:headerLen], uint32(len(payload)))
	copy(buf[headerLen:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame. A header announcing more than MaxPayload bytes
// is ErrFrameTooLarge (nothing more is read); a short read is
// io.ErrUnexpectedEOF, and EOF before any header byte is io.EOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxPayload {
		return Frame{}, ErrFrameTooLarge
	}
	f := Frame{Type: h[0]}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return f, nil
}

// Size is a terminal size in character cells. The zero Size means "no size".
type Size struct{ Rows, Cols uint16 }

func (s Size) zero() bool { return s.Rows == 0 || s.Cols == 0 }

func encodeU16s(vs ...uint16) []byte {
	b := make([]byte, 2*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint16(b[2*i:], v)
	}
	return b
}

// hello is a decoded hello frame.
type hello struct {
	version uint16
	size    Size // zero when the client sent no size
}

// parseHello decodes a hello payload: at least the version; the size is
// optional, and bytes after it are ignored so a later version can extend
// the frame.
func parseHello(p []byte) (hello, error) {
	if len(p) < 2 {
		return hello{}, errors.New("hello shorter than its version field")
	}
	h := hello{version: binary.BigEndian.Uint16(p)}
	if len(p) >= 6 {
		h.size = Size{binary.BigEndian.Uint16(p[2:]), binary.BigEndian.Uint16(p[4:])}
	}
	return h, nil
}

func parseSize(p []byte) (Size, error) {
	if len(p) != 4 {
		return Size{}, fmt.Errorf("resize payload is %d bytes, want 4", len(p))
	}
	return Size{binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:])}, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only fixed struct types are marshalled
	}
	return b
}
