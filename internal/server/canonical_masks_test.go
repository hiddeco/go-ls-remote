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
