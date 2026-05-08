package trace

import "time"

// PacketEvent is emitted by `pktline.Reader` and `pktline.Writer` for
// every pkt-line read or written when a [Tracer] is wired in.
//
// The Bytes field aliases the reader's or writer's internal buffer and
// is valid only for the duration of the [Tracer.OnEvent] call. Callers
// retaining the bytes across calls must copy, e.g. with [bytes.Clone].
//
// Bytes is nil for control packets (Kind values other than [PacketData]).
//
// Field order is chosen so 8-byte-aligned fields cluster ahead of the
// single-byte Direction and Kind, packing the struct without inter-
// field padding.
type PacketEvent struct {
	Time      time.Time
	URL       string // remote URL the packet belongs to; credentials redacted
	Bytes     []byte // payload bytes for data packets; nil for control packets
	Direction Direction
	Kind      PacketKind
}

// When implements [Event].
func (e PacketEvent) When() time.Time { return e.Time }

// HTTPEvent is emitted by the HTTP transport around each HTTP request:
// once per request after the response headers are received, or once with
// Status == 0 when the request fails before producing a response (dial
// error, TLS handshake failure, network timeout).
type HTTPEvent struct {
	Time     time.Time
	Method   string        // HTTP method, e.g. "GET" or "POST"
	URL      string        // request URL; credentials redacted
	Status   int           // HTTP status code, or 0 if no response was received
	Duration time.Duration // wall-clock time from request start to response headers (or to error)
	Err      error         // non-nil iff the request did not produce a response
}

// When implements [Event].
func (e HTTPEvent) When() time.Time { return e.Time }

// NegotiateEvent is emitted once per session, immediately after the
// initial capability advertisement is parsed. It records the protocol
// version actually negotiated with the server and the capabilities the
// server declared.
//
// Version is the raw protocol version (0, 1, or 2) as an int. It is
// deliberately NOT a `transport.ProtocolVersion`: this package is
// dependency-free, and the transport package imports trace for [Tracer].
// Callers comparing against the transport package's constants compare
// against the integer values directly.
type NegotiateEvent struct {
	Time         time.Time
	URL          string // server URL; credentials redacted
	Version      int    // negotiated protocol version: 0, 1, or 2
	ServerAgent  string // value of the server's `agent=` capability, or ""
	Capabilities []string
}

// When implements [Event].
func (e NegotiateEvent) When() time.Time { return e.Time }

// CommandEvent is emitted around each v2 command (currently `ls-refs`
// and `object-info`): once with Phase == [CommandStart] before the
// request is written, and once with Phase == [CommandEnd] after the
// response has been fully consumed (or the command has failed).
//
// Duration is zero on the start event; on the end event it is the
// wall-clock time elapsed between start and end.
//
// Err is nil on the start event; on the end event it is the error
// returned by the command, or nil if the command succeeded.
type CommandEvent struct {
	Time     time.Time
	URL      string       // server URL; credentials redacted
	Name     string       // command name, e.g. "ls-refs" or "object-info"
	Phase    CommandPhase // CommandStart or CommandEnd
	Duration time.Duration
	Err      error
}

// When implements [Event].
func (e CommandEvent) When() time.Time { return e.Time }

// CommandPhase distinguishes the start and end of a [CommandEvent]
// lifecycle.
type CommandPhase uint8

const (
	// CommandStart is set on the [CommandEvent] emitted before the
	// command request is written.
	CommandStart CommandPhase = iota + 1

	// CommandEnd is set on the [CommandEvent] emitted after the response
	// has been fully consumed or the command has failed.
	CommandEnd
)
