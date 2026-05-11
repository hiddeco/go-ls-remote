package server

import (
	"bytes"
	"errors"
	"io"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// agentToken is the fixed payload [maskAgent] substitutes for any
// `agent=<value>\n` data pkt-line. The token preserves the trailing
// LF so the substituted line is framing-identical to a canonical
// agent emission — only its byte content normalises.
const agentToken = "agent=$AGENT$\n"

// advertisementDroppedCaps lists the v2 capability names (matched as
// data pkt-line payload prefixes after the agent-value substitution)
// that [maskV2Advertisement] drops from the stream entirely. The set
// captures legitimate divergences between canonical Git 2.54 and
// this emulator:
//
//   - `fetch=` — canonical advertises `fetch=shallow wait-for-done`
//     by default (`upload-pack.c::upload_pack_v2_capabilities`); this
//     read-only emulator does not implement fetch and skips the
//     capability.
//   - `server-option` — canonical advertises by default
//     (`serve.c::server_option_advertise`); the emulator does not
//     service the optional extension.
//   - `object-info` — this emulator advertises unconditionally
//     because it implements the command; canonical 2.54 emits the
//     cap only under `feature.experimental` config and the corpus is
//     captured without it.
//
// The remaining caps (`agent`, `ls-refs`, `object-format`) must
// survive byte-identical so the harness asserts equivalence on the
// substantive common subset.
var advertisementDroppedCaps = [][]byte{
	[]byte("fetch="),
	[]byte("server-option"),
	[]byte("object-info"),
}

// maskAgent returns a copy of in with any data pkt-line whose payload
// begins with `agent=` replaced by `agent=$AGENT$\n`, with the 4-hex
// length prefix recomputed against the substituted payload. Flush
// (`0000`), delim (`0001`), and response-end (`0002`) packets, and
// any data pkt-line whose payload does not begin with `agent=`, are
// passed through byte-identical.
//
// maskAgent is the canonical-corpus byte-mask for the v2 capability
// advertisement's `agent` capability, whose value diverges between
// canonical Git (`git/<version>`) and this library
// (`lsremote/0` by default; see [github.com/hiddeco/go-ls-remote/internal/wire.DefaultUserAgent]).
// Canonical Git's `serve.c::agent_advertise` and this package's
// [writeV2Advertisement] are both compliant emissions; the mask
// normalises both sides to a common token so the byte-equivalence
// harness compares the substantive framing without flagging the
// agent string as a divergence.
//
// maskAgent is idempotent: applying it twice yields the same result
// as applying it once.
//
// If in cannot be parsed as a pkt-line stream, maskAgent returns a
// verbatim copy of in. The byte-equivalence harness will then
// surface the framing issue from the substantive comparison rather
// than half-rewriting the stream.
func maskAgent(in []byte) []byte {
	return applyMask(in, nil)
}

// maskV2Advertisement returns a copy of in with the agent line
// normalised (as [maskAgent] does) and the data pkt-lines for the
// capability names in [advertisementDroppedCaps] removed from the
// stream entirely. The remaining bytes — version line, framing,
// and the `agent` / `ls-refs` / `object-format` cap lines — survive
// byte-identical so the harness asserts byte-equivalence on the
// substantive common subset of the v2 capability advertisement.
//
// maskV2Advertisement is idempotent: applying it twice yields the
// same result as applying it once.
//
// If in cannot be parsed as a pkt-line stream, maskV2Advertisement
// returns a verbatim copy of in, leaving the framing surfacing to
// the substantive comparison rather than half-rewriting the stream.
func maskV2Advertisement(in []byte) []byte {
	return applyMask(in, advertisementDroppedCaps)
}

// applyMask is the shared implementation behind [maskAgent] and
// [maskV2Advertisement]: it walks in as a pkt-line stream and emits
// each packet to an internal buffer, substituting agent payloads
// with [agentToken] and dropping any data pkt-line whose payload
// begins with one of the prefixes in dropPrefixes. Control packets
// (flush, delim, response-end) pass through unchanged.
//
// dropPrefixes is matched against the raw payload — agent
// substitution happens before the drop check so a hypothetical
// `agent=` entry in dropPrefixes would never fire (the substituted
// line begins with `agent=$AGENT$`). The check exists to support
// future masks that drop entire capability lines (see
// [advertisementDroppedCaps]).
func applyMask(in []byte, dropPrefixes [][]byte) []byte {
	out := bytes.NewBuffer(make([]byte, 0, len(in)))
	w := pktline.NewWriter(out)
	r := pktline.NewReader(bytes.NewReader(in))
	for {
		pkt, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			return out.Bytes()
		}
		if err != nil {
			return bytes.Clone(in)
		}
		var werr error
		switch pkt.Kind {
		case pktline.Data:
			payload := pkt.Data
			if bytes.HasPrefix(payload, []byte("agent=")) {
				payload = []byte(agentToken)
			}
			if hasAnyPrefix(payload, dropPrefixes) {
				continue
			}
			werr = w.WritePacket(payload)
		case pktline.Flush:
			werr = w.WriteFlush()
		case pktline.Delim:
			werr = w.WriteDelim()
		case pktline.ResponseEnd:
			werr = w.WriteResponseEnd()
		}
		if werr != nil {
			return bytes.Clone(in)
		}
	}
}

// hasAnyPrefix returns true when payload starts with one of the byte
// slices in prefixes. The helper exists so [applyMask] can early-skip
// drop-listed data pkt-lines without a per-call allocation; a
// `slices.ContainsFunc` over `bytes.HasPrefix` would also work but is
// noisier at the callsite for a two-line predicate.
func hasAnyPrefix(payload []byte, prefixes [][]byte) bool {
	for _, p := range prefixes {
		if bytes.HasPrefix(payload, p) {
			return true
		}
	}
	return false
}
