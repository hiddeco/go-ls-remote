package wire

import "github.com/hiddeco/go-ls-remote/pktline"

// EncodeObjectInfo writes a v2 `object-info` command request to w.
// caps is the server's advertised capability set; userAgent overrides
// the library default [DefaultUserAgent] when non-empty. Each entry
// of oids is emitted as a separate `oid <hex>` line in caller-supplied
// order; the encoder does not validate OID format — server-side
// `protocol-caps.c::cap_object_info` rejects malformed values.
//
// The body grammar matches `gitprotocol-v2.adoc` §"object-info" lines
// 556-585: `command=object-info`, capability echo, `0001` delim, then
// the optional `size` line followed by one `oid <hex>` line per OID,
// terminated by a flush. Argument order is `size` first, OIDs after —
// the canonical server (`cap_object_info`) accepts any interleaving,
// but emitting `size` first matches the natural reading of the spec.
//
// EncodeObjectInfo does not flush the underlying writer — wrapping is
// left to the caller.
func EncodeObjectInfo(
	w *pktline.Writer,
	oids []string,
	args ObjectInfoArgs,
	caps RawCapabilities,
	userAgent string,
) error {
	if err := writeLine(w, "command=object-info"); err != nil {
		return err
	}
	if err := writeCapabilityEcho(w, caps, userAgent); err != nil {
		return err
	}
	if err := w.WriteDelim(); err != nil {
		return err
	}

	if args.Size {
		if err := writeLine(w, "size"); err != nil {
			return err
		}
	}
	for _, oid := range oids {
		if err := writeLine(w, "oid "+oid); err != nil {
			return err
		}
	}

	return w.WriteFlush()
}
