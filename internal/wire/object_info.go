package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
)

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
		if err := w.WriteLineParts("oid ", oid); err != nil {
			return err
		}
	}

	return w.WriteFlush()
}

// DecodeObjectInfo reads a v2 `object-info` response from r and returns the
// per-OID information in stream order. Per `protocol-caps.c::send_info`
// (lines 37-77), the response takes one of three shapes:
//
//   - Bare flush — the client requested zero OIDs (`send_info:44-45`).
//     Returns `nil, nil`.
//   - Attrs PKT-LINE followed by `<oid>[ <size>]` rows — the client
//     requested at least one attribute (only `size` is currently
//     defined). The attrs line is `size\n` per `send_info:47-48`.
//   - Per-OID rows with no attrs PKT-LINE — the client requested no
//     attributes; `send_info:47-48` skips the attrs emission entirely
//     and `send_info:63` writes just `<oid>\n` per OID.
//
// The third shape diverges from the `gitprotocol-v2.adoc` §"object-info"
// grammar (lines 573-585), which lists the attrs PKT-LINE as
// non-optional. The decoder follows canonical Git's actual emission
// rather than the spec text and identifies the shape from the first
// data packet: a line whose first space-delimited token is a
// canonical-length hex OID (40 chars for SHA-1, 64 for SHA-256) is
// treated as a per-OID row; anything else is treated as the attrs
// line.
//
// An OID the server cannot resolve in its odb is emitted as `<oid> `
// with an empty size field (`send_info:66-67`). DecodeObjectInfo drops
// such entries from the returned slice; callers can detect a missing
// OID by comparing input requests against the result.
//
// DecodeObjectInfo does not close r; the caller owns its lifetime.
func DecodeObjectInfo(r *pktline.Reader) ([]RawObjectInfo, error) {
	pkt, err := r.ReadPacket()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch pkt.Kind {
	case pktline.Flush:
		return nil, nil
	case pktline.Delim, pktline.ResponseEnd:
		return nil, fmt.Errorf(
			"wire: unexpected control packet %v in object-info response", pkt.Kind)
	}

	firstLine := bytes.TrimSuffix(pkt.Data, []byte{'\n'})

	// `ERR ` detection per `pkt-line.c:509-510`. The shared
	// [CheckERRPacket] helper wraps [ErrServerRefused] so callers
	// can match the sentinel via `errors.Is`.
	if errPkt := CheckERRPacket(firstLine); errPkt != nil {
		return nil, errPkt
	}

	var (
		infos    []RawObjectInfo
		wantSize bool
	)

	if looksLikeObjectInfoOIDLine(firstLine) {
		// `send_info:47-48` skipped the attrs PKT-LINE because the
		// client requested no attributes; the first packet is already
		// a per-OID row. wantSize stays false — the no-attrs branch is
		// only reachable when `size` was not requested.
		info, drop, err := parseObjectInfoLine(string(firstLine), false)
		if err != nil {
			return nil, err
		}
		if !drop {
			infos = append(infos, info)
		}
	} else {
		// Tokenise `attrs = attr | attrs SP attrs` (lines 576-577).
		// Today the only defined attribute is `size`; record whether
		// the response will carry a size token on each per-OID line.
		for tok := range strings.FieldsSeq(string(firstLine)) {
			if tok == "size" {
				wantSize = true
			}
		}
	}

	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		switch pkt.Kind {
		case pktline.Flush:
			return infos, nil
		case pktline.Delim, pktline.ResponseEnd:
			return nil, fmt.Errorf(
				"wire: unexpected control packet %v in object-info response", pkt.Kind)
		}

		line := bytes.TrimSuffix(pkt.Data, []byte{'\n'})

		// `ERR ` detection per `pkt-line.c:509-510`. The shared
		// [CheckERRPacket] helper wraps [ErrServerRefused].
		if errPkt := CheckERRPacket(line); errPkt != nil {
			return nil, errPkt
		}

		info, drop, err := parseObjectInfoLine(string(line), wantSize)
		if err != nil {
			return nil, err
		}
		if drop {
			continue
		}
		infos = append(infos, info)
	}
}

// looksLikeObjectInfoOIDLine reports whether line is shaped like a
// per-OID row from `protocol-caps.c::send_info`. The first
// space-delimited token of a per-OID row is the lowercase-hex object
// id, of length 40 (SHA-1) or 64 (SHA-256). No currently-defined
// attribute name (only `size` exists today) collides with that shape,
// and an empty attrs PKT-LINE (`\n`) yields an empty token that is
// also distinguishable.
//
// The check lets [DecodeObjectInfo] tell the no-attrs shape (per-OID
// row first) from the attrs-bearing shape (`size\n` or `\n` first)
// without out-of-band knowledge of whether the client asked for the
// `size` argument.
func looksLikeObjectInfoOIDLine(line []byte) bool {
	tok, _, _ := bytes.Cut(line, []byte{' '})
	if len(tok) != 40 && len(tok) != 64 {
		return false
	}
	for _, c := range tok {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// parseObjectInfoLine parses a single trimmed v2 `object-info` per-OID
// line. The wire shape is governed by `protocol-caps.c::send_info` (lines
// 40-77 of canonical Git's `protocol-caps.c`), which emits one of three
// forms when `size` is in play:
//
//   - `<oid> <number>` — object found, size known.
//   - `<oid> `         — object NOT in the server's odb (trailing space,
//     empty size); we drop these so callers see only
//     successfully-resolved OIDs.
//   - `<oid>`          — `size` was not requested by the client.
//
// drop reports whether the caller should skip appending the returned
// [RawObjectInfo] (used for the missing-object case).
func parseObjectInfoLine(line string, wantSize bool) (info RawObjectInfo, drop bool, err error) {
	oid, tail, hasSpace := strings.Cut(line, " ")
	// `send_info` never emits an empty OID: every per-OID line in the
	// canonical server's output begins with the lowercase hex object id.
	// Treat an empty OID as a malformed line and drop it rather than
	// surface a row with no usable identifier.
	if oid == "" {
		return RawObjectInfo{}, true, nil
	}
	if !hasSpace {
		// No space at all. If the response did not advertise `size`,
		// this is the canonical no-attribute shape. If it did, the
		// server should have emitted a trailing space for a missing
		// object; absent that, treat the line as malformed and drop
		// silently rather than poison the batch.
		if wantSize {
			return RawObjectInfo{}, true, nil
		}
		return RawObjectInfo{OID: oid}, false, nil
	}

	if tail == "" {
		// `<oid> ` shape — `send_info` writes the trailing space when
		// `odb_read_object_info` fails. Drop.
		return RawObjectInfo{}, true, nil
	}

	if !wantSize {
		// Forward-compat: a future attribute appears in the tail but
		// the decoder did not see it in the attrs line. Surface the
		// OID with a zero size; the unknown token is silently dropped.
		return RawObjectInfo{OID: oid}, false, nil
	}

	size, perr := strconv.ParseInt(tail, 10, 64)
	if perr != nil {
		return RawObjectInfo{}, false,
			fmt.Errorf("wire: malformed object-info size %q: %w", tail, perr)
	}
	return RawObjectInfo{OID: oid, Size: size}, false, nil
}
