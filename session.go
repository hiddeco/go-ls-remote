package lsremote

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Session represents an open discovery-time connection to a remote Git
// repository. A Session is produced by [Dial] after the handshake
// completes; methods on a Session issue one or more discovery commands
// — `ls-refs`, `object-info` — against the same underlying connection.
//
// # Opaque type
//
// Session has no exported fields. Callers obtain a `*Session` from
// [Dial] and interact with it only through its methods; the internal
// state — the live [transport.Conn], the negotiated [Capabilities],
// the cached advertisement-time refs (for v0/v1 only), the captured
// dial configuration and the redacted URL — is library-private.
//
// # Concurrency
//
// A `*Session` is safe for concurrent use only when the underlying
// transport multiplexes independent commands onto independent network
// requests. The HTTP transport satisfies this: each v2 command is its
// own POST, so two goroutines can issue commands against the same
// HTTP-backed Session without external synchronisation. The SSH,
// `git://`, and `file://` transports do NOT satisfy it: they share a
// single bidirectional byte stream where one in-flight command must
// drain before the next begins. Callers using a non-HTTP transport
// must serialise Session method calls externally.
//
// # Lifecycle
//
// A Session owns the underlying [transport.Conn]. [Session.Close]
// releases that connection; repeated calls return nil per the
// [transport.Conn] idempotent-Close contract.
type Session struct {
	// conn is the live transport-level connection. It owns the
	// advertisement reader (already consumed by [Dial]) and is the
	// channel through which future v2 commands flow.
	conn transport.Conn

	// caps is the negotiated capability snapshot built from the
	// advertisement. [Session.Capabilities] hands callers a deep copy.
	caps Capabilities

	// rawCaps is the verbatim wire-order capability list captured at
	// [Dial] time. It feeds the v2 command encoders
	// ([wire.EncodeLSRefs], [wire.EncodeObjectInfo]) which need the
	// slice shape for `caps.All("ls-refs")` (unborn-gate lookup) and
	// `caps.Has("agent")` / `caps.Get("object-format")` (cap-echo). The
	// public [Capabilities.Raw] map loses wire order so it cannot
	// substitute. Allocated once at construction; never mutated.
	rawCaps wire.RawCapabilities

	// refs holds the advertisement-time ref list for v0/v1 handshakes.
	// v2 leaves it nil — v2 callers fetch refs on demand via an
	// `ls-refs` command. The slice is allocated by [convertRefs] so it
	// does not alias any wire-layer buffer.
	refs []Ref

	// config is the resolved dial configuration. It is captured at
	// [Dial] time so later Session methods can reuse the configured
	// tracer, user agent, and protocol pin when issuing v2 commands.
	config dialConfig

	// url is the credential-redacted form of the URL [Dial] was called
	// with. It is stored once so each subsequent [ProtocolError] does
	// not need to re-derive the redaction.
	url string
}

// Capabilities returns a deep copy of the capability snapshot captured
// at [Dial] time. The copy is independent of the Session's internal
// state: mutating any slice or map on the returned value cannot affect
// the result of a later call.
//
// The fields are populated per the rules documented on [Capabilities]
// itself.
//
// # Allocation
//
// `Capabilities` allocates a fresh deep copy on every call. Callers in
// hot loops should cache the returned value rather than calling this
// method repeatedly.
func (s *Session) Capabilities() Capabilities {
	return cloneCapabilities(s.caps)
}

// Refs returns an iterator over the server's references.
//
// On v2 ([ProtocolV2]) Refs issues an `ls-refs` command, forwarding
// [RefsRequest.Prefixes] verbatim as `ref-prefix` arguments and
// toggling `peel`, `symrefs`, and `unborn` per the corresponding
// [RefsRequest] flags. The `unborn` argument is dropped silently when
// the server did not advertise `ls-refs=unborn` in its capability list
// ([connect.c::get_remote_refs lines 564-597]). The iterator streams
// the response and yields one (Ref, error) pair per emission.
//
// On v0/v1 the wire has no `ls-refs` equivalent: the full ref
// advertisement has already been consumed by [Dial] and cached on the
// Session. Refs filters that cached slice by [RefsRequest.Prefixes]
// client-side and yields the survivors. [RefsRequest.Peel] and
// [RefsRequest.Unborn] are still no-ops on v0/v1: peeled-tag
// information rides inline on the v0/v1 advertisement regardless, and
// v0/v1 has no unborn-`HEAD` wire representation.
//
// [RefsRequest.Symrefs] is honoured client-side on v0/v1 even though
// it has no wire effect: when the flag is true the library post-fills
// [Ref.Symref] on each yielded ref from [Capabilities.Symrefs],
// unifying the call-site experience with v2. When the flag is false,
// [Ref.Symref] is always empty on yielded refs, regardless of what the
// advertisement carried. [Capabilities.Symrefs] remains populated in
// both cases for callers who prefer the capability-level view.
//
// The returned error is non-nil only when the v2 command request
// itself fails before any response bytes are consumed (for example a
// transport-level POST failure). A wire-level decode error mid-stream
// surfaces through the iterator: it yields a single (zero [Ref], err)
// pair wrapping the cause in a `*ProtocolError` with `Op == "ls-refs"`
// and stops. Iteration over a successful v0/v1 path never yields an
// error.
//
// [connect.c::get_remote_refs lines 564-597]: https://github.com/git/git/blob/v2.54.0/connect.c#L564-L597
func (s *Session) Refs(ctx context.Context, args RefsRequest) (iter.Seq2[Ref, error], error) {
	if s.caps.Version != ProtocolV2 {
		return s.refsCached(args), nil
	}
	return s.refsV2(ctx, args)
}

// refsCached yields the advertisement-time ref slice filtered by
// `args.Prefixes`. Used by the v0/v1 paths where the wire has no
// equivalent of `ls-refs` and the full ref list was already captured
// at [Dial] time.
//
// When `args.Symrefs` is true, the yielded [Ref.Symref] is populated
// from the capability-level advertisement (`s.caps.Symrefs`) for any
// ref whose name appears in that list — matching the v0/v1 wire shape
// described in [connect.c::parse_one_symref_info]. This unifies the
// call-site experience with v2, where the `symrefs` argument to
// `ls-refs` causes the server to include per-ref symref targets inline.
//
// When `args.Symrefs` is false, [Ref.Symref] is always empty on the
// yielded copy, even when the underlying advertisement carried the
// information. This preserves the pre-existing contract: the symref
// mapping is available at all times on [Capabilities.Symrefs] for
// callers who prefer that view, but per-ref fields are opt-in.
//
// The cached [Session.refs] slice is never mutated; only the copy
// that is passed to yield is adjusted.
//
// [connect.c::parse_one_symref_info]: https://github.com/git/git/blob/v2.54.0/connect.c#L183
func (s *Session) refsCached(args RefsRequest) iter.Seq2[Ref, error] {
	return func(yield func(Ref, error) bool) {
		for _, ref := range s.refs {
			if !matchPrefixes(ref.Name, args.Prefixes) {
				continue
			}
			out := ref
			if args.Symrefs {
				// Post-fill Ref.Symref from the capability-level
				// advertisement when the caller opted in. The wire layer
				// already applies symrefs to RawRef at parse time (via
				// [connect.c::annotate_refs_with_symref_info]), so the
				// cached entry carries the value; we expose it here only
				// when Symrefs is set, matching the opt-in semantics of
				// the v2 path.
				//
				// [connect.c::annotate_refs_with_symref_info]: https://github.com/git/git/blob/v2.54.0/connect.c#L209
				if out.Symref == "" {
					out.Symref = s.symrefTarget(ref.Name)
				}
			} else {
				// Clear any capability-level symref that rode in from
				// the wire cache so callers that did not request symref
				// resolution observe an empty field.
				out.Symref = ""
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}

// symrefTarget returns the capability-level symref target for name,
// matching the v0/v1 advertisement shape captured in
// [connect.c::parse_one_symref_info lines 183-207]. A linear scan
// is acceptable because Capabilities.Symrefs is typically tiny on
// v0/v1 servers — often just `HEAD → refs/heads/main`. Returns the
// empty string when no entry matches.
//
// [connect.c::parse_one_symref_info lines 183-207]: https://github.com/git/git/blob/v2.54.0/connect.c#L183-L207
func (s *Session) symrefTarget(name string) string {
	for _, sr := range s.caps.Symrefs {
		if sr.Name == name {
			return sr.Target
		}
	}
	return ""
}

// refsV2 issues an `ls-refs` command and returns an iterator over the
// streamed response. Errors are wrapped in `*ProtocolError` with
// `Op == "ls-refs"` and the session's negotiated version on `Version`.
func (s *Session) refsV2(ctx context.Context, args RefsRequest) (iter.Seq2[Ref, error], error) {
	wireArgs := wire.RefsArgs{
		Prefixes: args.Prefixes,
		Peel:     args.Peel,
		Symrefs:  args.Symrefs,
		Unborn:   args.Unborn,
	}
	rdr, err := s.conn.Command(ctx, "ls-refs", func(w *pktline.Writer) error {
		return wire.EncodeLSRefs(w, wireArgs, s.rawCaps, s.config.userAgent, s.config.tracer)
	})
	if err != nil {
		return nil, s.protocolError("ls-refs", err)
	}

	seq := wire.DecodeLSRefs(rdr)
	return func(yield func(Ref, error) bool) {
		// drained tracks whether the inner range exited at the
		// trailing flush packet. When the caller breaks the public
		// iterator before that point, [drainLSRefsResponse] runs on
		// the deferred path to advance rdr past the flush — the
		// `file://`, `git://`, and `ssh://` transports require each
		// command's response to drain before the next request flows
		// on the same byte stream ([connect.c::get_remote_refs lines 564-597]).
		//
		// [connect.c::get_remote_refs lines 564-597]: https://github.com/git/git/blob/v2.54.0/connect.c#L564-L597
		drained := false
		defer func() {
			if !drained {
				drainLSRefsResponse(rdr)
			}
		}()
		for rr, err := range seq {
			if err != nil {
				// Mid-stream error: the rdr position is ill-defined
				// (a parse error mid-line may have left it misaligned),
				// and a server still writing the response would block a
				// drain. Skip the drain — the session is effectively
				// dead and callers should [Session.Close] it.
				drained = true
				yield(Ref{}, s.protocolError("ls-refs", err))
				return
			}
			if !yield(convertRef(rr), nil) {
				// Caller broke early. The deferred drain will advance
				// rdr to the trailing flush before this goroutine
				// returns.
				return
			}
		}
		// Natural exhaustion: [wire.DecodeLSRefs] consumed the
		// trailing flush before returning.
		drained = true
	}, nil
}

// drainLSRefsResponse advances rdr past the trailing flush packet
// that ends a v2 `ls-refs` response. Used when the iterator returned
// by [Session.Refs] is abandoned before the flush so the underlying
// [transport.Conn] is left in the drained state subsequent commands
// require — load-bearing on the `file://`, `git://`, and `ssh://`
// transports where one bidirectional byte stream carries every
// command, and useful on HTTP where it lets the response body return
// to the connection pool.
//
// The drain is best-effort: any read error stops the loop and is
// swallowed, because the iterator is being abandoned anyway and a
// follow-up command will surface the broken stream on its own. The
// response shape is `*ref flush-pkt` per
// [gitprotocol-v2.adoc §"ls-refs" output (lines 231-239)], so any
// non-flush packet is silently skipped and the loop terminates on
// the first flush.
//
// [gitprotocol-v2.adoc §"ls-refs" output (lines 231-239)]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc?plain=1#L231-L239
func drainLSRefsResponse(rdr *pktline.Reader) {
	for {
		pkt, err := rdr.ReadPacket()
		if err != nil {
			return
		}
		if pkt.Kind == pktline.Flush {
			return
		}
	}
}

// ListRefs collects the refs yielded by [Session.Refs] into a slice.
// Iteration stops on the first error, which is returned alongside a
// nil slice; otherwise every yielded ref is appended in order.
func (s *Session) ListRefs(ctx context.Context, args RefsRequest) ([]Ref, error) {
	seq, err := s.Refs(ctx, args)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for ref, err := range seq {
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// ObjectInfo issues a v2 `object-info` command and returns the per-OID
// metadata the server replied with.
//
// `object-info` is a v2-only command. A Session whose
// [Capabilities.Version] is not [ProtocolV2], or whose v2
// [Capabilities.Commands] set does not include `object-info`, returns
// a [*ProtocolError] with `Op == "object-info"` whose error chain
// matches [ErrUnsupportedProtocol]. The capability-set guard mirrors
// canonical Git's pre-issue check: mainstream hosts advertise v2 with
// only `ls-refs` and `fetch`, so a client-side short-circuit is the
// only way to surface a public-typed error rather than a raw
// transport failure.
//
// The `oids` slice is forwarded verbatim as one `oid <hex>` argument
// per element. When `args.Size` is true the request also carries the
// `size` argument and each returned [ObjectInfo.Size] is populated
// with the server's reported size. When `args.Size` is false the
// Session sets every returned [ObjectInfo.Size] to `-1`, matching the
// "size not requested" sentinel documented on the [ObjectInfo] type
// itself. The library cannot distinguish a real zero-byte blob from a
// size the server elided on a Size-requested call, so a returned
// `Size == 0` is left as-is in the Size-requested branch.
//
// `ObjectInfo` returns a slice, not an iterator, because the response
// is bounded by the caller-supplied `oids` count and can be safely
// materialised in memory. `Session.Refs`, by contrast, returns an
// iterator: an `ls-refs` response can be arbitrarily large and should
// be streamed rather than buffered.
func (s *Session) ObjectInfo(ctx context.Context, oids []string,
	args ObjectInfoRequest) ([]ObjectInfo, error) {
	if s.caps.Version != ProtocolV2 {
		return nil, &ProtocolError{
			URL:     s.url,
			Op:      "object-info",
			Version: versionPtr(s.caps.Version),
			Server:  "object-info requires protocol v2",
			Err:     ErrUnsupportedProtocol,
		}
	}
	if !slices.Contains(s.caps.Commands, "object-info") {
		return nil, &ProtocolError{
			URL:     s.url,
			Op:      "object-info",
			Version: versionPtr(s.caps.Version),
			Server:  "server did not advertise object-info",
			Err:     ErrUnsupportedProtocol,
		}
	}

	wireArgs := wire.ObjectInfoArgs{Size: args.Size}
	rdr, err := s.conn.Command(ctx, "object-info", func(w *pktline.Writer) error {
		return wire.EncodeObjectInfo(w, oids, wireArgs, s.rawCaps, s.config.userAgent)
	})
	if err != nil {
		return nil, s.protocolError("object-info", err)
	}

	raw, err := wire.DecodeObjectInfo(rdr)
	if err != nil {
		return nil, s.protocolError("object-info", err)
	}

	out := convertObjectInfos(raw)
	if !args.Size {
		// The public contract says `-1` for "size not requested";
		// canonical Git's [protocol-caps.c::send_info] omits the size
		// column entirely on this branch and the wire decoder leaves
		// `Size == 0`, which we translate to the public sentinel here.
		//
		// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
		for i := range out {
			out[i].Size = -1
		}
	}
	return out, nil
}

// Close releases the underlying [transport.Conn]. The contract on
// [transport.Conn.Close] requires the implementation to be idempotent,
// so a second or later call after the first returns nil.
func (s *Session) Close() error {
	return s.conn.Close()
}

// matchPrefixes reports whether name is admitted by the prefix filter.
// An empty list admits every name, matching [ls-refs.c::ref_match].
// Otherwise name matches when at least one prefix is a string-prefix
// of name.
//
// [ls-refs.c::ref_match]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L54
func matchPrefixes(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// protocolError wraps err in a [*ProtocolError] tagged with the
// Session's redacted URL and negotiated version. The wrapped error is
// stored on `Err` so `errors.Is`/`errors.As` walks through to it.
//
// [wire.ErrServerRefused] is the one mid-session sentinel that needs
// joining here: an `ERR <msg>` pkt-line arrives during the v2 command
// response and we want public callers to match
// `errors.Is(err, ErrServerRefused)` without walking the wire layer.
// Mid-session refusals (an unknown command, an `ERR` pkt-line
// mid-stream, an unrecognised `ls-refs` argument) all flow through
// this hook. [wire.ErrUnsupportedProtocol] is joined at
// advertisement-parse time in [bridgeOpenError] inside `dial.go`, not
// here.
func (s *Session) protocolError(op string, err error) error {
	wrapped := err
	if errors.Is(err, wire.ErrServerRefused) {
		wrapped = errors.Join(ErrServerRefused, err)
	}
	return &ProtocolError{
		URL:     s.url,
		Op:      op,
		Version: versionPtr(s.caps.Version),
		Err:     wrapped,
	}
}

// versionPtr returns a fresh pointer to v. Used by Session error paths
// so the [*ProtocolError.Version] field carries the negotiated version
// without aliasing the Session's own copy.
func versionPtr(v ProtocolVersion) *ProtocolVersion {
	out := v
	return &out
}

// cloneCapabilities returns a deep copy of c. Every slice and map is
// freshly allocated so the caller can mutate the returned value
// without affecting the source.
func cloneCapabilities(c Capabilities) Capabilities {
	out := Capabilities{
		Version:        c.Version,
		Agent:          c.Agent,
		ObjectFormat:   c.ObjectFormat,
		Commands:       slices.Clone(c.Commands),
		LSRefsArgs:     slices.Clone(c.LSRefsArgs),
		ObjectInfoArgs: slices.Clone(c.ObjectInfoArgs),
		Symrefs:        slices.Clone(c.Symrefs),
	}
	if c.Raw != nil {
		out.Raw = make(map[string][]string, len(c.Raw))
		for k, v := range c.Raw {
			out.Raw[k] = slices.Clone(v)
		}
	}
	return out
}
