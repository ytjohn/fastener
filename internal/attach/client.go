package attach

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// DialTimeout bounds connecting and the hello/welcome exchange.
const DialTimeout = 5 * time.Second

// Options for Dial.
type Options struct {
	// Size is the client terminal's size, applied to the pty on attach. The
	// zero Size attaches without resizing (and without forcing a redraw).
	Size Size
	// Version overrides the protocol version sent in the hello (tests).
	Version uint16
}

// Client is one attached terminal. Send, Resize and Detach may be called
// from different goroutines than Recv.
type Client struct {
	conn net.Conn
	wmu  sync.Mutex
	// Welcome is the server's answer to the hello.
	Welcome Welcome
}

// Welcome is the decoded welcome frame.
type Welcome struct {
	Version int
	Size    Size // the pty's size after the attach
}

// Message is one frame received from the server: Data (terminal output) or
// Exit (the session ended; the server then closes the connection).
type Message struct {
	Type byte
	Data []byte
	Exit *ExitInfo
}

// Dial connects to the attach socket at path and performs the handshake.
// A server refusal (wrong version, too many clients) is a *RemoteError.
func Dial(path string, opt Options) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, DialTimeout)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	v := opt.Version
	if v == 0 {
		v = Version
	}
	_ = conn.SetDeadline(time.Now().Add(DialTimeout))
	if err := WriteFrame(conn, TypeHello, encodeU16s(v, opt.Size.Rows, opt.Size.Cols)); err != nil {
		conn.Close()
		return nil, err
	}
	f, err := ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("attach: no welcome: %w", err)
	}
	switch f.Type {
	case TypeWelcome:
	case TypeError:
		conn.Close()
		return nil, remoteError(f.Payload)
	default:
		conn.Close()
		return nil, fmt.Errorf("attach: expected welcome, got frame type %d", f.Type)
	}
	if len(f.Payload) < 6 {
		conn.Close()
		return nil, fmt.Errorf("attach: welcome is %d bytes, want 6", len(f.Payload))
	}
	c.Welcome = Welcome{
		Version: int(binary.BigEndian.Uint16(f.Payload)),
		Size:    Size{binary.BigEndian.Uint16(f.Payload[2:]), binary.BigEndian.Uint16(f.Payload[4:])},
	}
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

func remoteError(p []byte) error {
	var e RemoteError
	if err := json.Unmarshal(p, &e.ErrorInfo); err != nil {
		return fmt.Errorf("attach: malformed error frame: %w", err)
	}
	return &e
}

func (c *Client) write(typ byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.conn, typ, p)
}

// Send writes terminal input, split into frames of at most MaxPayload bytes.
func (c *Client) Send(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), MaxPayload)
		if err := c.write(TypeData, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Resize tells the server the client's terminal size changed.
func (c *Client) Resize(s Size) error { return c.write(TypeResize, encodeU16s(s.Rows, s.Cols)) }

// Detach leaves the session (it keeps running) and closes the connection.
func (c *Client) Detach() error {
	err := c.write(TypeDetach, nil)
	if cerr := c.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// DetachWithin is Detach bounded by d: the write deadline is set before
// anything else, so even a Send blocked on a full connection gives up (and
// releases the frame lock) within d. The connection is closed in any case.
func (c *Client) DetachWithin(d time.Duration) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(d))
	err := c.write(TypeDetach, nil)
	if cerr := c.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close closes the connection without a detach frame (the server treats it
// the same way).
func (c *Client) Close() error { return c.conn.Close() }

// SetReadDeadline sets a deadline for Recv.
func (c *Client) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// Recv returns the next message. An error frame from the server is returned
// as a *RemoteError; the end of the connection as io.EOF.
func (c *Client) Recv() (Message, error) {
	f, err := ReadFrame(c.conn)
	if err != nil {
		return Message{}, err
	}
	switch f.Type {
	case TypeData:
		return Message{Type: TypeData, Data: f.Payload}, nil
	case TypeExit:
		var e ExitInfo
		if err := json.Unmarshal(f.Payload, &e); err != nil {
			return Message{}, fmt.Errorf("attach: malformed exit frame: %w", err)
		}
		return Message{Type: TypeExit, Exit: &e}, nil
	case TypeError:
		return Message{}, remoteError(f.Payload)
	default:
		return Message{}, fmt.Errorf("attach: unexpected frame type %d from the server", f.Type)
	}
}
