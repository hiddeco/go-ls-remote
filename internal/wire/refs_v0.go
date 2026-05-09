package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// peeledSuffix is the canonical `^{}` marker used to dereference an
// annotated tag onto its target object on a v0/v1 ref line
// (`gitprotocol-pack.adoc` lines 219-228, `other-peeled` production).
const peeledSuffix = "^{}"

// emptyRepoRefname is the placeholder name advertised on the lone ref
// line of an empty repository: `<zero-oid> capabilities^{}`. Canonical
// Git's `connect.c::process_dummy_ref` (lines 260-274) recognises and
// skips this synthetic entry.
const emptyRepoRefname = "capabilities" + peeledSuffix

// shallowPrefix is the leading marker on a `shallow <oid>` advertisement
// line (`gitprotocol-pack.adoc` `shallow` production, lines 233-235).
// The `ls-remote` shape never asks for shallow data, but a defensive
// parser still tolerates the line if a server emits it.
const shallowPrefix = "shallow "

// parseV0Advertisement reads the body of a v0 or v1 advertisement and
// returns the corresponding [Advertisement]. version is the negotiated
// [transport.ProtocolV0] or [transport.ProtocolV1].
//
// first is the cap-bearing first ref line: for v0 it is the data
// packet the discriminator already peeked; for v1 it is the data
// packet that followed the consumed `version 1` line. The contract
// from [dispatchAdvertisement] guarantees `first.Kind == pktline.Data`
// — empty (control-only) advertisements are short-circuited there.
//
// r is positioned to read the *next* packet after first. The function
// reads up to and including the terminating flush packet but does not
// close r; the caller owns its lifetime.
//
// The grammar followed is `gitprotocol-pack.adoc` lines 219-228:
//
//	first-ref    = obj-id SP refname NUL capability-list
//	other-tip    = obj-id SP refname
//	other-peeled = obj-id SP refname "^{}"
//	no-refs      = zero-id SP "capabilities^{}" NUL capability-list
//
// Trailing LF on each pkt-line payload is part of the framing convention
// and is stripped before parsing. A `shallow <oid>` line emitted under
// the `*shallow` production is silently skipped.
//
// After the ref list is built, every `symref=NAME:TARGET` capability is
// applied to the matching ref's [RawRef.Symref] field, mirroring
// canonical Git's `connect.c::annotate_refs_with_symref_info`
// (lines 209-233).
func parseV0Advertisement(first pktline.Packet, r *pktline.Reader, version transport.ProtocolVersion) (Advertisement, error) {
	caps, firstRef, err := parseFirstRefLine(first.Data)
	if err != nil {
		return Advertisement{}, err
	}

	var refs []RawRef
	if firstRef != nil {
		refs = append(refs, *firstRef)
	}

	for {
		p, err := r.ReadPacket()
		if err != nil {
			// `connect.c:75` treats EOF before the terminating flush as
			// a truncated advertisement; surface it as
			// [io.ErrUnexpectedEOF] for uniform caller handling.
			if errors.Is(err, io.EOF) {
				return Advertisement{}, io.ErrUnexpectedEOF
			}
			return Advertisement{}, err
		}
		if p.Kind == pktline.Flush {
			break
		}
		if p.Kind != pktline.Data {
			return Advertisement{}, fmt.Errorf(
				"wire: unexpected control packet in v0/v1 advertisement")
		}

		line := bytes.TrimSuffix(p.Data, []byte{'\n'})
		// `connect.c::process_shallow` (lines 311 onward) handles the
		// `shallow <oid>` production; we drop it because the ls-remote
		// shape never negotiates shallow.
		if bytes.HasPrefix(line, []byte(shallowPrefix)) {
			continue
		}

		oid, name, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			return Advertisement{}, fmt.Errorf(
				"wire: malformed ref line %q: missing space", line)
		}

		if peeledName, isPeeled := strings.CutSuffix(string(name), peeledSuffix); isPeeled {
			if len(refs) == 0 {
				return Advertisement{}, fmt.Errorf(
					"wire: peeled marker %q has no preceding ref", line)
			}
			prev := &refs[len(refs)-1]
			if prev.Name != peeledName {
				return Advertisement{}, fmt.Errorf(
					"wire: peeled marker for %q does not match preceding ref %q",
					peeledName, prev.Name)
			}
			if prev.Peeled != "" {
				return Advertisement{}, fmt.Errorf(
					"wire: duplicate peeled marker for ref %q", prev.Name)
			}
			prev.Peeled = string(oid)
			continue
		}

		refs = append(refs, RawRef{OID: string(oid), Name: string(name)})
	}

	applySymrefs(refs, caps)
	return Advertisement{Version: version, Caps: caps, Refs: refs}, nil
}

// parseFirstRefLine splits the cap-bearing first ref line into its
// capability list and (when present) the leading [RawRef]. The empty-
// repo placeholder `<zero-oid> capabilities^{}` returns a nil ref so
// the caller does not record it — `connect.c::process_dummy_ref`
// (lines 260-274) does the same in canonical Git.
func parseFirstRefLine(payload []byte) (RawCapabilities, *RawRef, error) {
	line := bytes.TrimSuffix(payload, []byte{'\n'})

	oid, rest, ok := bytes.Cut(line, []byte{' '})
	if !ok {
		return nil, nil, fmt.Errorf(
			"wire: malformed first ref line %q: missing space", line)
	}
	name, capList, ok := bytes.Cut(rest, []byte{0})
	if !ok {
		return nil, nil, fmt.Errorf(
			"wire: malformed first ref line %q: missing NUL before caps", line)
	}

	caps := ParseCapabilities(string(capList))

	if string(name) == emptyRepoRefname && isAllZero(oid) {
		return caps, nil, nil
	}
	if bytes.HasSuffix(name, []byte(peeledSuffix)) {
		return nil, nil, fmt.Errorf(
			"wire: first ref line %q carries peeled marker without preceding ref",
			line)
	}
	return caps, &RawRef{OID: string(oid), Name: string(name)}, nil
}

// applySymrefs walks every `symref=NAME:TARGET` capability and copies
// TARGET onto the matching ref's [RawRef.Symref] field. The split is
// on the first `:` only, matching `connect.c::parse_one_symref_info`
// (lines 183-207). A symref naming a refname not present in the
// advertisement is silently ignored.
func applySymrefs(refs []RawRef, caps RawCapabilities) {
	for _, val := range caps.All("symref") {
		name, target, ok := strings.Cut(val, ":")
		if !ok || name == "" || target == "" {
			continue
		}
		for i := range refs {
			if refs[i].Name == name {
				refs[i].Symref = target
				break
			}
		}
	}
}

// isAllZero reports whether every byte of b is the ASCII digit `0`.
// The empty-repo placeholder advertises a zero object id whose hex
// length depends on the negotiated hash algorithm; a length-agnostic
// byte check covers SHA-1 (40) and SHA-256 (64) without committing to
// either.
func isAllZero(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c != '0' {
			return false
		}
	}
	return true
}
