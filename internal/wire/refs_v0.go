package wire

import (
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// parseV0Advertisement reads the body of a v0 or v1 advertisement.
// version is the negotiated [transport.ProtocolV0] or
// [transport.ProtocolV1]. For v0, first is the data packet that already
// discriminated as not-a-version-line and IS the first ref line. For
// v1, first is the first packet AFTER the `version 1` line — i.e. the
// cap-bearing first ref line.
//
// TODO: full ref + caps parsing lands in the next task. The stub
// returns the version-only advertisement so the discriminator can
// already be tested in isolation.
func parseV0Advertisement(first pktline.Packet, r *pktline.Reader, version transport.ProtocolVersion) (Advertisement, error) {
	_, _ = first, r
	return Advertisement{Version: version}, nil
}
