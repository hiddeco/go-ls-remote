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
		if err := writeLine(w, "oid "+oid); err != nil {
			return err
		}
	}

	return w.WriteFlush()
}

// DecodeObjectInfo reads a v2 `object-info` response from r and returns the
// per-OID information in stream order. The first data packet is the `attrs`
// line echoing which attributes the response carries (`size` is the only
// currently-defined attribute per `gitprotocol-v2.adoc`). Subsequent data
// packets are `<oid> <size>` lines.
//
// Per `protocol-caps.c::send_info`, an OID the server cannot resolve in
// its odb is emitted as `<oid> ` with an empty size field. DecodeObjectInfo
// drops such entries from the returned slice; callers can detect a missing
// OID by comparing input requests against the result.
//
// DecodeObjectInfo does not close r; the caller owns its lifetime.
func DecodeObjectInfo(r *pktline.Reader) ([]RawObjectInfo, error) {
	// First data packet: attrs line. Treat any control packet here as a
	// wire violation — `gitprotocol-v2.adoc` §"object-info" output (lines
	// 573-585) requires the server emit at least the attrs PKT-LINE.
	attrsPkt, err := r.ReadPacket()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	switch attrsPkt.Kind {
	case pktline.Flush:
		return nil, errors.New("wire: object-info response missing attrs line")
	case pktline.Delim, pktline.ResponseEnd:
		return nil, fmt.Errorf(
			"wire: unexpected control packet %v in object-info response", attrsPkt.Kind)
	}

	attrsLine := bytes.TrimSuffix(attrsPkt.Data, []byte{'\n'})

	// Inline `ERR ` detection per `pkt-line.c:509-510`. A shared sentinel
	// that lifts the detection above each command decoder is a follow-up.
	if msg, ok := bytes.CutPrefix(attrsLine, []byte("ERR ")); ok {
		return nil, fmt.Errorf("wire: server returned ERR: %s", msg)
	}

	// Tokenise `attrs = attr | attrs SP attrs` (lines 576-577). Today the
	// only defined attribute is `size`; record whether the response will
	// carry a size token on each per-OID line.
	wantSize := false
	for tok := range strings.FieldsSeq(string(attrsLine)) {
		if tok == "size" {
			wantSize = true
		}
	}

	var infos []RawObjectInfo
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

		// Inline `ERR ` detection per `pkt-line.c:509-510`.
		if msg, ok := bytes.CutPrefix(line, []byte("ERR ")); ok {
			return nil, fmt.Errorf("wire: server returned ERR: %s", msg)
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
