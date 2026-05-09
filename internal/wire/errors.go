package wire

import "errors"

// ErrUnsupportedProtocol is the wire-internal sentinel returned when
// the server's advertisement negotiates a protocol version the caller
// did not ask for, or when the server's first packet announces a
// version this client does not speak — the literal `version 0`, an
// unparseable integer, or any other unknown value. The root `lsremote`
// package wraps this sentinel into its public `*ProtocolError` shape;
// inside the wire package, callers match with [errors.Is].
var ErrUnsupportedProtocol = errors.New("wire: unsupported protocol version")

// ErrServerRefused is the wire-internal sentinel returned when a
// decoder observes an `ERR <message>` data pkt-line per
// `pkt-line.c:509-510`. The wrapped error carries the server's
// message text after the four-byte `ERR ` prefix. The root `lsremote`
// package re-wraps this sentinel into its public `*ProtocolError`
// shape with the operation context; inside the wire package, callers
// match with [errors.Is].
var ErrServerRefused = errors.New("wire: server refused")
