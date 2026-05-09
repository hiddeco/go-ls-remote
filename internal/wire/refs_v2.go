package wire

import (
	"time"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// EncodeLSRefs writes a v2 `ls-refs` command request to w. caps is
// the server's advertised capability set. userAgent overrides the
// library default [DefaultUserAgent] when non-empty. tracer is
// optional; a nil tracer disables event emission.
//
// The capability echo and `unborn` gate follow canonical Git
// (`connect.c::send_capabilities` lines 490-516,
// `connect.c::get_remote_refs` lines 564-597), and the body grammar
// matches `gitprotocol-v2.adoc` §"command-request".
//
// EncodeLSRefs does not flush the underlying writer — wrapping is
// left to the caller.
func EncodeLSRefs(
	w *pktline.Writer,
	args RefsArgs,
	caps RawCapabilities,
	userAgent string,
	tracer trace.Tracer,
) error {
	if err := writeLine(w, "command=ls-refs"); err != nil {
		return err
	}

	// Capability echo: order mirrors `send_capabilities` —
	// `agent` before `object-format`. We skip `promisor-remote`
	// because the ls-remote shape never asks for it.
	if caps.Has("agent") {
		ua := userAgent
		if ua == "" {
			ua = DefaultUserAgent
		}
		if err := writeLine(w, "agent="+ua); err != nil {
			return err
		}
	}
	// `server_feature_v2` (connect.c:97) requires the capability to
	// carry a `=value`, so a boolean `object-format` token is not
	// echoed.
	if v, ok := caps.Get("object-format"); ok && v != "" {
		if err := writeLine(w, "object-format="+v); err != nil {
			return err
		}
	}

	if err := w.WriteDelim(); err != nil {
		return err
	}

	// Argument order matches `get_remote_refs`: peel, symrefs,
	// unborn, ref-prefix.
	if args.Peel {
		if err := writeLine(w, "peel"); err != nil {
			return err
		}
	}
	if args.Symrefs {
		if err := writeLine(w, "symrefs"); err != nil {
			return err
		}
	}
	if args.Unborn {
		if lsRefsSupportsUnborn(caps) {
			if err := writeLine(w, "unborn"); err != nil {
				return err
			}
		} else {
			trace.Emit(tracer, CapabilityDropEvent{
				Time:     time.Now(),
				Command:  "ls-refs",
				Argument: "unborn",
				Reason:   "server did not advertise ls-refs=unborn",
			})
		}
	}
	for _, p := range args.Prefixes {
		if err := writeLine(w, "ref-prefix "+p); err != nil {
			return err
		}
	}

	return w.WriteFlush()
}

// writeLine emits a single pkt-line whose payload is s followed by
// the literal `LF` byte. The `LF` is part of every command, capability,
// and argument line in the v2 request grammar
// (`gitprotocol-v2.adoc` §"command-request").
func writeLine(w *pktline.Writer, s string) error {
	return w.WritePacket([]byte(s + "\n"))
}

// lsRefsSupportsUnborn reports whether any `ls-refs` capability value
// advertised by the server contains the `unborn` feature token. The
// scan mirrors `server_supports_feature("ls-refs", "unborn", 0)` at
// `connect.c:112-132`, which walks every `ls-refs[=value]` line and
// runs `parse_feature_request` on each value.
//
// Each value is itself a whitespace-separated feature list; reusing
// [ParseCapabilities] gives the same tokenisation canonical Git's
// `parse_feature_value` performs. A boolean `ls-refs` (no value)
// contributes no tokens and so does not enable the gate.
func lsRefsSupportsUnborn(caps RawCapabilities) bool {
	for _, v := range caps.All("ls-refs") {
		for _, sub := range ParseCapabilities(v) {
			if sub.Name == "unborn" {
				return true
			}
		}
	}
	return false
}
