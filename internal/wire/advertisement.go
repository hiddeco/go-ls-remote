package wire

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// versionPrefix is the canonical `version ` marker that opens a v1 or
// v2 server advertisement (canonical Git's [protocol.c:89],
// `skip_prefix(server_response, "version ", ...)`).
//
// [protocol.c:89]: https://github.com/git/git/blob/v2.54.0/protocol.c#L89
const versionPrefix = "version "

// ParseAdvertisement reads the initial advertisement from r and returns
// the negotiated [Advertisement]. want pins the protocol version: a
// nil pointer accepts whatever the server speaks, while a non-nil
// pointer constrains the negotiated version, returning a wrapped
// [ErrUnsupportedProtocol] if the server answers with anything else.
//
// ParseAdvertisement does not close r; the caller owns its lifetime.
//
// Discrimination follows canonical Git's [connect.c::discover_version
// (lines 143-181)] and [protocol.c::determine_protocol_version_client
// (lines 85-99)]: peek the first packet, route on its kind and — for a
// data packet — on whether it begins with `version `, then either
// consume a v2 capability list or hand the v0/v1 body to the
// ref-line parser.
//
// For v0 and v1 the first ref line carries the capability list and
// is therefore consumed by the ref-line parser, not here; this
// function returns the [Advertisement] that parser produced, with
// [Advertisement.Version] already set.
//
// [connect.c::discover_version (lines 143-181)]: https://github.com/git/git/blob/v2.54.0/connect.c#L143-L181
// [protocol.c::determine_protocol_version_client (lines 85-99)]: https://github.com/git/git/blob/v2.54.0/protocol.c#L85-L99
func ParseAdvertisement(r *pktline.Reader, want *transport.ProtocolVersion) (Advertisement, error) {
	first, err := r.ReadPacket()
	if err != nil {
		// [connect.c:152-153] treats `PACKET_READ_EOF` as an unexpected hangup
		// (`die_initial_contact`); a clean [io.EOF] before any byte is
		// the same condition for callers, but we surface it as
		// [io.ErrUnexpectedEOF] to convey "advertisement was truncated"
		// uniformly. Other errors propagate verbatim.
		//
		// [connect.c:152-153]: https://github.com/git/git/blob/v2.54.0/connect.c#L152-L153
		if errors.Is(err, io.EOF) {
			return Advertisement{}, io.ErrUnexpectedEOF
		}
		return Advertisement{}, err
	}

	ad, err := dispatchAdvertisement(first, r)
	if err != nil {
		return Advertisement{}, err
	}
	if want != nil && ad.Version != *want {
		return Advertisement{}, fmt.Errorf(
			"wire: server negotiated %s, caller required %s: %w",
			ad.Version, *want, ErrUnsupportedProtocol)
	}
	return ad, nil
}

// dispatchAdvertisement routes the first peeked packet to the correct
// version-specific reader. The split keeps [ParseAdvertisement]'s
// `want` enforcement at one site.
func dispatchAdvertisement(first pktline.Packet, r *pktline.Reader) (Advertisement, error) {
	// Control packets at the head map to v0 with no refs and no caps —
	// [connect.c:154-157] collapses Flush, Delim, and Response-End into
	// `protocol_v0`.
	//
	// [connect.c:154-157]: https://github.com/git/git/blob/v2.54.0/connect.c#L154-L157
	if first.Kind != pktline.Data {
		return Advertisement{Version: transport.ProtocolV0}, nil
	}

	// [protocol.c:89]: a leading `version ` selects v1/v2 (or rejects
	// the explicit `version 0` and unknown digits below). Anything
	// else is a v0 advertisement whose first packet IS the first ref
	// line, so it must be forwarded to the ref-line parser intact.
	//
	// [protocol.c:89]: https://github.com/git/git/blob/v2.54.0/protocol.c#L89
	payload := strings.TrimSuffix(string(first.Data), "\n")
	rest, isVersionLine := strings.CutPrefix(payload, versionPrefix)
	if !isVersionLine {
		return parseV0Advertisement(first, r, transport.ProtocolV0)
	}

	switch rest {
	case "2":
		caps, err := readV2Capabilities(r)
		if err != nil {
			return Advertisement{}, err
		}
		return Advertisement{Version: transport.ProtocolV2, Caps: caps}, nil
	case "1":
		// The peeked `version 1` line was consumed by `r.ReadPacket()`
		// above, so the next packet is the first ref line — pass that
		// through. [connect.c:168-171] mirrors this "read past the version
		// line then continue" shape for v1.
		//
		// [connect.c:168-171]: https://github.com/git/git/blob/v2.54.0/connect.c#L168-L171
		next, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Advertisement{}, io.ErrUnexpectedEOF
			}
			return Advertisement{}, err
		}
		return parseV0Advertisement(next, r, transport.ProtocolV1)
	case "0":
		// [protocol.c:95]: `die("protocol error: server explicitly said
		// version 0")`. Surface as [ErrUnsupportedProtocol] so callers
		// match with [errors.Is].
		//
		// [protocol.c:95]: https://github.com/git/git/blob/v2.54.0/protocol.c#L95
		return Advertisement{}, fmt.Errorf(
			"wire: server explicitly announced version 0: %w",
			ErrUnsupportedProtocol)
	default:
		// [protocol.c:93]: `die("server is speaking an unknown
		// protocol")` — any non-{0,1,2} digit or non-numeric tail.
		//
		// [protocol.c:93]: https://github.com/git/git/blob/v2.54.0/protocol.c#L93
		return Advertisement{}, fmt.Errorf(
			"wire: server announced unknown protocol version %q: %w",
			rest, ErrUnsupportedProtocol)
	}
}

// readV2Capabilities consumes the v2 capability advertisement: a
// sequence of data pkt-lines, each carrying one `name` or `name=value`
// token, terminated by a flush packet. Canonical Git's
// [connect.c::process_capabilities_v2 (lines 134-141)] reads each line
// and feeds it to the capability list verbatim, stopping on Flush.
//
// The trailing LF on each cap line is part of the pkt-line framing —
// the cap text is the bytes before LF — so it is stripped before the
// payload is handed to [ParseCapabilities].
//
// [connect.c::process_capabilities_v2 (lines 134-141)]: https://github.com/git/git/blob/v2.54.0/connect.c#L134-L141
func readV2Capabilities(r *pktline.Reader) (RawCapabilities, error) {
	var caps RawCapabilities
	for {
		p, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if p.Kind == pktline.Flush {
			return caps, nil
		}
		if p.Kind != pktline.Data {
			return nil, fmt.Errorf(
				"wire: unexpected control packet in v2 capability list")
		}
		line := strings.TrimSuffix(string(p.Data), "\n")
		caps = append(caps, ParseCapabilities(line)...)
	}
}
