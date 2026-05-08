package trace

// PacketKind identifies the structural role of a single pkt-line in the
// Git wire protocol. It mirrors `pktline.Kind`; the duplication keeps
// this package free of import cycles, since the
// [github.com/hiddeco/go-ls-remote/pktline] package imports this package
// for [Tracer]. The pktline package converts its own `pktline.Kind`
// values to PacketKind at every emission site.
//
// The on-wire 4-byte length prefix that determines a pkt-line's kind is
// documented in `Documentation/gitprotocol-common.adoc` of canonical
// Git: `0000` is the flush packet, `0001` the delimiter, `0002` the
// response-end marker; any other value is a normal data packet whose
// payload length is the prefix value minus four.
type PacketKind uint8

const (
	// PacketData identifies a normal data packet: a pkt-line carrying
	// arbitrary payload bytes. The on-wire length prefix is in the
	// inclusive range 0004..fff0.
	PacketData PacketKind = iota + 1

	// PacketFlush identifies the flush control packet, on-wire `0000`.
	// In Git's protocol it terminates a section of a request or response.
	PacketFlush

	// PacketDelim identifies the delimiter control packet, on-wire `0001`.
	// In v2 command requests it separates the capability list from the
	// command-specific arguments; see canonical Git's
	// `Documentation/gitprotocol-v2.adoc` §"Command Request".
	PacketDelim

	// PacketResponseEnd identifies the response-end control packet,
	// on-wire `0002`. In v2 sideband-multiplexed responses it marks the
	// final packet of a server response.
	PacketResponseEnd
)
