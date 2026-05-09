package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// EncodeLSRefs writes a v2 `ls-refs` command request to w. caps is
// the server's advertised capability set. userAgent overrides the
// library default [DefaultUserAgent] when non-empty. tracer is
// optional; a nil tracer disables event emission.
//
// Capability echo is delegated to [writeCapabilityEcho]; the `unborn`
// gate follows `connect.c::get_remote_refs` lines 564-597, and the
// body grammar matches `gitprotocol-v2.adoc` §"command-request".
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
	if err := writeCapabilityEcho(w, caps, userAgent); err != nil {
		return err
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

// DecodeLSRefs reads a v2 `ls-refs` response from r and yields each
// parsed [RawRef] in stream order. The iterator stops on flush, on a
// caller `yield` returning false, on a transport read error, or on a
// server `ERR` packet. Errors are yielded as a (zero [RawRef], err)
// pair; the iterator yields no further values after an error.
//
// The grammar matches `gitprotocol-v2.adoc` §"ls-refs" output (lines
// 231-244):
//
//	output           = *ref flush-pkt
//	obj-id-or-unborn = (obj-id | "unborn")
//	ref              = PKT-LINE(obj-id-or-unborn SP refname *(SP ref-attribute) LF)
//	ref-attribute    = (symref | peeled)
//
// Per `connect.c::process_ref_v2` (lines 395-470), unknown attributes
// are silently ignored — a forward-compatibility hook canonical Git
// relies on. OID hashes are passed through verbatim; hash-length
// validation is the root package's concern.
//
// DecodeLSRefs does not close r; the caller owns its lifetime.
func DecodeLSRefs(r *pktline.Reader) iter.Seq2[RawRef, error] {
	return func(yield func(RawRef, error) bool) {
		for {
			pkt, err := r.ReadPacket()
			if err != nil {
				// `process_ref_v2` returns from `get_remote_refs`
				// (connect.c:564-597) on EOF before flush; surface that
				// as `io.ErrUnexpectedEOF` for uniform caller handling.
				if errors.Is(err, io.EOF) {
					yield(RawRef{}, io.ErrUnexpectedEOF)
					return
				}
				yield(RawRef{}, err)
				return
			}
			switch pkt.Kind {
			case pktline.Flush:
				return
			case pktline.Delim, pktline.ResponseEnd:
				yield(RawRef{}, fmt.Errorf(
					"wire: unexpected control packet %v in ls-refs response", pkt.Kind))
				return
			}

			line := bytes.TrimSuffix(pkt.Data, []byte{'\n'})

			// Inline `ERR ` detection per `pkt-line.c:509-510`.
			// Surface the payload text directly so callers get a
			// usable diagnostic; a shared sentinel that lifts the
			// detection above each command decoder is a follow-up.
			if msg, ok := bytes.CutPrefix(line, []byte("ERR ")); ok {
				yield(RawRef{}, fmt.Errorf("wire: server returned ERR: %s", msg))
				return
			}

			ref, perr := parseLSRefsLine(line)
			if perr != nil {
				yield(RawRef{}, perr)
				return
			}
			if !yield(ref, nil) {
				return
			}
		}
	}
}

// parseLSRefsLine parses a single trimmed v2 `ls-refs` ref line into a
// [RawRef]. The token rules mirror `connect.c::process_ref_v2` lines
// 395-470: split on spaces, treat a leading `unborn` as the OID-stand-in
// for an unborn ref, and silently ignore tokens whose prefix is not
// `peeled:` or `symref-target:`.
func parseLSRefsLine(line []byte) (RawRef, error) {
	tokens := strings.Fields(string(line))
	if len(tokens) < 2 {
		return RawRef{}, errors.New(
			"wire: malformed ls-refs ref line: expected at least 2 fields")
	}

	if tokens[0] == "unborn" {
		ref := RawRef{Name: tokens[1], Unborn: true}
		for _, tok := range tokens[2:] {
			if t, ok := strings.CutPrefix(tok, "symref-target:"); ok {
				ref.Symref = t
			}
			// Other attributes (including a stray `peeled:` on an
			// unborn ref) are silently ignored — `process_ref_v2`
			// only checks for `symref-target:` in the unborn branch.
		}
		return ref, nil
	}

	ref := RawRef{OID: tokens[0], Name: tokens[1]}
	for _, tok := range tokens[2:] {
		if t, ok := strings.CutPrefix(tok, "peeled:"); ok {
			ref.Peeled = t
			continue
		}
		if t, ok := strings.CutPrefix(tok, "symref-target:"); ok {
			ref.Symref = t
			continue
		}
		// Unknown attribute — silently dropped, mirroring the trailing
		// `else` arm in `process_ref_v2`.
	}
	return ref, nil
}
