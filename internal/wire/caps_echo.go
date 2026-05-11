package wire

import "github.com/hiddeco/go-ls-remote/pktline"

// writeCapabilityEcho emits the `agent=` and `object-format=` lines a
// v2 command request echoes back when the server advertised them.
// Order matches `connect.c::send_capabilities` lines 490-516: `agent`
// first, then `object-format`. `promisor-remote` is intentionally
// omitted — the discovery flows in this package never request it.
//
// userAgent overrides the library default [DefaultUserAgent] when
// non-empty. A boolean `agent` token still triggers the echo (per
// `server_feature_v1` semantics: any advertisement of `agent` lets the
// client introduce its own UA), while a boolean `object-format` token
// is dropped — `server_feature_v2` (connect.c:97) requires the
// capability to carry a `=value`.
//
// The helper does not write any framing other than the capability
// pkt-lines themselves; the caller owns the surrounding command line,
// delim, and flush.
func writeCapabilityEcho(w *pktline.Writer, caps RawCapabilities, userAgent string) error {
	if caps.Has("agent") {
		ua := userAgent
		if ua == "" {
			ua = DefaultUserAgent
		}
		if err := w.WriteLineParts("agent=", ua); err != nil {
			return err
		}
	}
	if v, ok := caps.Get("object-format"); ok && v != "" {
		if err := w.WriteLineParts("object-format=", v); err != nil {
			return err
		}
	}
	return nil
}
