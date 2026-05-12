// Package pktline implements the pkt-line wire-format codec used by the
// Git smart protocol in versions 0, 1, and 2.
//
// # Wire format
//
// Each pkt-line begins with a 4-byte ASCII hexadecimal length prefix
// covering the prefix itself plus payload. Three reserved length
// values denote control packets that carry no payload: `0000` (flush),
// `0001` (delimiter), `0002` (response-end). Data packets use length
// values in the inclusive range `0004`..`fff0`; the payload is
// `length - 4` bytes long.
//
// The format is specified in canonical Git's
// [Documentation/gitprotocol-common.adoc] and implemented in
// `pkt-line.c`.
//
// # Tracer integration
//
// [Reader] and [Writer] accept optional [github.com/hiddeco/go-ls-remote/trace.Tracer]
// hooks via [WithReaderTracer] and [WithWriterTracer]. When wired in,
// every packet read or written emits a `trace.PacketEvent`. Without a
// tracer, the codec performs no instrumentation work.
//
// [Documentation/gitprotocol-common.adoc]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-common.adoc
package pktline

// MaxPayload is the largest payload, in bytes, a single pkt-line may
// carry. It equals canonical Git's `LARGE_PACKET_MAX` (65520; see
// `pkt-line.h`) minus the 4-byte length prefix.
const MaxPayload = 65516

// Kind identifies the structural role of a single pkt-line.
//
// Zero is [Data] so a zero-valued [Packet] represents a normal data
// packet with empty payload — the same shape produced when the wire
// length prefix is `0004`.
type Kind uint8

const (
	// Data identifies a normal data packet whose Data field carries
	// the payload bytes. The on-wire length prefix is in the inclusive
	// range `0004`..`fff0`.
	Data Kind = iota

	// Flush identifies the flush control packet, on-wire `0000`. It
	// terminates a section of a request or response.
	Flush

	// Delim identifies the delimiter control packet, on-wire `0001`.
	// In v2 command requests it separates the capability list from the
	// command-specific arguments.
	Delim

	// ResponseEnd identifies the response-end control packet, on-wire
	// `0002`. In v2 sideband-multiplexed responses it marks the final
	// packet of a server response.
	ResponseEnd
)

// Packet is the value yielded by [Reader.ReadPacket].
//
// For data packets (Kind == [Data]), Data carries the payload bytes
// and aliases the [Reader]'s internal buffer; it is valid only until
// the next call to ReadPacket. Callers that retain the bytes across
// calls must copy them, for example with [bytes.Clone].
//
// For control packets (Kind != [Data]), Data is nil.
type Packet struct {
	Kind Kind
	Data []byte
}
