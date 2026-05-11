package lsremote

import (
	"context"
	"iter"
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
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
// (`connect.c::get_remote_refs` lines 564-597). The iterator streams
// the response and yields one (Ref, error) pair per emission.
//
// On v0/v1 the wire has no `ls-refs` equivalent: the full ref
// advertisement has already been consumed by [Dial] and cached on the
// Session. Refs filters that cached slice by [RefsRequest.Prefixes]
// client-side and yields the survivors. [RefsRequest.Peel],
// [RefsRequest.Symrefs], and [RefsRequest.Unborn] are no-ops on
// v0/v1: peeled-tag information rides inline on the v0/v1
// advertisement regardless, symref targets surface on
// [Capabilities.Symrefs] rather than per-ref, and v0/v1 has no
// unborn-`HEAD` wire representation.
//
// The returned error is non-nil only when the v2 command request
// itself fails before any response bytes are consumed (for example a
// transport-level POST failure). A wire-level decode error mid-stream
// surfaces through the iterator: it yields a single (zero [Ref], err)
// pair wrapping the cause in a `*ProtocolError` with `Op == "ls-refs"`
// and stops. Iteration over a successful v0/v1 path never yields an
// error.
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
func (s *Session) refsCached(args RefsRequest) iter.Seq2[Ref, error] {
	return func(yield func(Ref, error) bool) {
		for _, ref := range s.refs {
			if !matchPrefixes(ref.Name, args.Prefixes) {
				continue
			}
			if !yield(ref, nil) {
				return
			}
		}
	}
}

// refsV2 issues an `ls-refs` command and returns an iterator over the
// streamed response. Errors are wrapped in `*ProtocolError` with
// `Op == "ls-refs"` and the session's negotiated version on `Version`.
func (s *Session) refsV2(ctx context.Context, args RefsRequest) (iter.Seq2[Ref, error], error) {
	cmdArgs := buildLSRefsArgs(args, s.caps)
	cmdCaps := buildCommandCaps(s.caps, s.config.userAgent)

	rdr, err := s.conn.Command(ctx, "ls-refs", cmdArgs, cmdCaps)
	if err != nil {
		return nil, s.protocolError("ls-refs", err)
	}

	seq := wire.DecodeLSRefs(rdr)
	return func(yield func(Ref, error) bool) {
		for rr, err := range seq {
			if err != nil {
				yield(Ref{}, s.protocolError("ls-refs", err))
				return
			}
			if !yield(convertRef(rr), nil) {
				return
			}
		}
	}, nil
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
// [Capabilities.Version] is not [ProtocolV2] returns a [*ProtocolError]
// with `Op == "object-info"` whose error chain matches
// [ErrUnsupportedProtocol].
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

	cmdArgs := buildObjectInfoArgs(oids, args)
	cmdCaps := buildCommandCaps(s.caps, s.config.userAgent)

	rdr, err := s.conn.Command(ctx, "object-info", cmdArgs, cmdCaps)
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
		// canonical Git's `protocol-caps.c::send_info` omits the size
		// column entirely on this branch and the wire decoder leaves
		// `Size == 0`, which we translate to the public sentinel here.
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

// buildLSRefsArgs translates the public [RefsRequest] into the
// command-args string slice the transport's `Conn.Command` expects.
// Argument order matches `connect.c::get_remote_refs` lines 564-597:
// `peel`, `symrefs`, `unborn` (gated by the server advertising
// `ls-refs=unborn` per `connect.c::server_supports_feature` lines
// 112-132), then one `ref-prefix <p>` per element of `args.Prefixes`.
func buildLSRefsArgs(args RefsRequest, caps Capabilities) []string {
	var out []string
	if args.Peel {
		out = append(out, "peel")
	}
	if args.Symrefs {
		out = append(out, "symrefs")
	}
	if args.Unborn && lsRefsAdvertisesUnborn(caps) {
		out = append(out, "unborn")
	}
	for _, p := range args.Prefixes {
		out = append(out, "ref-prefix "+p)
	}
	return out
}

// buildObjectInfoArgs translates the public [ObjectInfoRequest] plus
// the OID slice into the command-args string slice the transport's
// `Conn.Command` expects. `size` is emitted first when requested,
// mirroring the natural reading of `gitprotocol-v2.adoc`
// §"object-info"; canonical Git's `protocol-caps.c::cap_object_info`
// accepts any interleaving.
func buildObjectInfoArgs(oids []string, args ObjectInfoRequest) []string {
	out := make([]string, 0, len(oids)+1)
	if args.Size {
		out = append(out, "size")
	}
	for _, oid := range oids {
		out = append(out, "oid "+oid)
	}
	return out
}

// buildCommandCaps assembles the capability-echo lines the transport
// emits in the capability-list portion of a v2 command request. The
// shape mirrors the wire layer's `writeCapabilityEcho`
// (`internal/wire/caps_echo.go`): `agent=<ua>` first when the server
// advertised `agent`, then `object-format=<v>` when the server
// advertised it with a non-empty value. `promisor-remote` is omitted —
// the discovery surface never requests it.
//
// `userAgent` overrides the library default when non-empty. The
// `wire.DefaultUserAgent` value is reused so a Session built without
// `WithUserAgent` emits the same agent string the wire-layer encoder
// would have emitted directly.
func buildCommandCaps(caps Capabilities, userAgent string) []string {
	var out []string
	if _, ok := caps.Raw["agent"]; ok {
		ua := userAgent
		if ua == "" {
			ua = wire.DefaultUserAgent
		}
		out = append(out, "agent="+ua)
	}
	if of := string(caps.ObjectFormat); of != "" {
		out = append(out, "object-format="+of)
	}
	return out
}

// lsRefsAdvertisesUnborn reports whether the server's `ls-refs`
// capability advertisement carries the `unborn` feature token. The
// scan mirrors `internal/wire.lsRefsSupportsUnborn` but reads the
// public [Capabilities.LSRefsArgs] slice rather than the raw
// capability list — the splitting has already happened in
// [convertCaps]. A boolean `ls-refs` advertisement leaves
// [Capabilities.LSRefsArgs] empty and so does not enable the gate.
func lsRefsAdvertisesUnborn(caps Capabilities) bool {
	for _, arg := range caps.LSRefsArgs {
		if arg == "unborn" {
			return true
		}
	}
	return false
}

// matchPrefixes reports whether name is admitted by the prefix filter.
// An empty list admits every name, matching
// `connect.c::ref_match`/`ls-refs.c::ref_match`. Otherwise name
// matches when at least one prefix is a string-prefix of name.
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
func (s *Session) protocolError(op string, err error) error {
	return &ProtocolError{
		URL:     s.url,
		Op:      op,
		Version: versionPtr(s.caps.Version),
		Err:     err,
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
		FetchArgs:      slices.Clone(c.FetchArgs),
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
