// Package proto defines the wire protocol between the zerock agent and server.
//
// A session starts as a plain TCP connection (usually wrapped in TLS). The
// agent sends a length-prefixed JSON Hello, the server answers with a
// HelloAck, and from that point both sides hand the connection to yamux. The
// server is the yamux client (it opens streams); the agent is the yamux server
// (it accepts them).
//
// Every stream opens with a single kind byte so the agent knows whether it is
// carrying proxied traffic or server-pushed events.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Version is the control protocol version. The server rejects a mismatch.
const Version = 1

// maxMsg caps a single handshake frame so a hostile peer cannot make us
// allocate without bound.
const maxMsg = 1 << 20

// StreamKind is the first byte of every yamux stream.
type StreamKind byte

const (
	// KindData carries proxied bytes for one request or TCP connection.
	KindData StreamKind = 0x01
	// KindEvents carries a one-way JSON stream of Events, server to agent.
	KindEvents StreamKind = 0x02
)

// TunnelType is the kind of traffic a tunnel forwards.
type TunnelType string

const (
	TypeHTTP TunnelType = "http"
	TypeTCP  TunnelType = "tcp"
)

// Hello is the agent's opening frame.
type Hello struct {
	V         int        `json:"v"`
	Token     string     `json:"token"`
	Type      TunnelType `json:"type"`
	Subdomain string     `json:"sub,omitempty"`
	LocalHost string     `json:"local_host,omitempty"`
	LocalPort int        `json:"local_port"`
	// RemotePort requests a specific public port for a TCP tunnel. Zero lets
	// the server assign one from its configured range.
	RemotePort int `json:"remote_port,omitempty"`
	// BasicAuth, when set to "user:pass", makes the edge demand HTTP basic
	// auth before a request reaches the agent.
	BasicAuth string `json:"basic_auth,omitempty"`
	Agent     string `json:"agent"`
	Hostname  string `json:"hostname,omitempty"`
}

// HelloAck is the server's answer. OK false means the tunnel was refused and
// the connection is about to close.
type HelloAck struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`

	TunnelID      string `json:"tunnel_id,omitempty"`
	Subdomain     string `json:"sub,omitempty"`
	URL           string `json:"url,omitempty"`
	PublicAddr    string `json:"public_addr,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
}

// Error codes returned in HelloAck.Error.
const (
	ErrBadRequest      = "bad_request"
	ErrUnauthorized    = "unauthorized"
	ErrVersionMismatch = "version_mismatch"
	ErrSubTaken        = "subdomain_taken"
	ErrSubReserved     = "subdomain_reserved"
	ErrSubBlocked      = "subdomain_blocked"
	ErrSubInvalid      = "subdomain_invalid"
	ErrTunnelLimit     = "tunnel_limit"
	ErrNoPorts         = "no_ports_available"
	ErrInternal        = "internal"
)

// Event types pushed on the KindEvents stream.
const (
	EventRequest = "req"    // an HTTP request completed at the edge
	EventConn    = "conn"   // a TCP connection opened or closed
	EventNotice  = "notice" // human-readable server message
	EventClose   = "close"  // the server is shutting this tunnel down
)

// Event is one entry on the event stream. Fields not relevant to the type are
// omitted.
type Event struct {
	T       string    `json:"t"`
	At      time.Time `json:"at"`
	Method  string    `json:"method,omitempty"`
	Path    string    `json:"path,omitempty"`
	Status  int       `json:"status,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
	Millis  float64   `json:"ms,omitempty"`
	Remote  string    `json:"remote,omitempty"`
	Message string    `json:"message,omitempty"`
	// Final marks an EventClose the agent must not retry past: the tunnel was
	// ended deliberately rather than interrupted.
	Final bool `json:"final,omitempty"`
}

// WriteMsg writes v as a length-prefixed JSON frame.
func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxMsg {
		return fmt.Errorf("proto: message too large (%d bytes)", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadMsg reads one length-prefixed JSON frame into v.
func ReadMsg(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("proto: empty message")
	}
	if n > maxMsg {
		return fmt.Errorf("proto: message too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
