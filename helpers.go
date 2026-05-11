package lsremote

import (
	"context"
	"errors"
	"iter"
)

// Refs is the one-shot form of [Session.Refs]. It dials the remote at
// rawURL, requests an `ls-refs` stream (or filters the v0/v1
// advertisement-time cache, per [Session.Refs]'s version split), and
// arranges for the underlying [Session] to close when the returned
// iterator is exhausted or abandoned.
//
// The Session lifetime is tied to the iterator: the wrapper's deferred
// [Session.Close] runs when the inner range-over-func returns, whether
// via exhaustion of the source stream or via the caller's `break` (the
// latter surfaces as `yield` returning false). Callers should not hold
// the iterator across goroutines or store it for later draining.
//
// On a dial-time or `ls-refs`-time failure Refs returns `(nil, err)`
// and leaves no Session open.
func Refs(ctx context.Context, rawURL string, args RefsRequest,
	opts ...Option) (iter.Seq2[Ref, error], error) {
	s, err := Dial(ctx, rawURL, opts...)
	if err != nil {
		return nil, err
	}
	seq, err := s.Refs(ctx, args)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	// Wrap seq so the Session closes when the caller is done iterating.
	// Go's range-over-func resumes the inner `for` after `yield` returns,
	// so `defer s.Close()` runs on both normal exhaustion and an early
	// stop via `!yield`.
	wrapped := func(yield func(Ref, error) bool) {
		defer func() { _ = s.Close() }()
		for ref, err := range seq {
			if !yield(ref, err) {
				return
			}
		}
	}
	return wrapped, nil
}

// ListRefs is the one-shot form of [Session.ListRefs]. It dials,
// collects every ref into a slice, and closes the underlying [Session]
// before returning.
//
// On a dial-time or `ls-refs`-time failure ListRefs returns `(nil,
// err)` and leaves no Session open.
func ListRefs(ctx context.Context, rawURL string, args RefsRequest,
	opts ...Option) ([]Ref, error) {
	s, err := Dial(ctx, rawURL, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.ListRefs(ctx, args)
}

// ObjectInfos is the one-shot form of [Session.ObjectInfo]. It dials,
// issues an `object-info` command, and closes the underlying [Session]
// before returning. The plural name avoids the Go-level collision
// with the [ObjectInfo] response type while keeping the relationship
// with [Session.ObjectInfo] obvious at the call site.
//
// `object-info` is a v2-only command. A server that negotiates v0 or
// v1 (or a v2 server that did not advertise `object-info`) produces a
// [*ProtocolError] whose chain matches [ErrUnsupportedProtocol], per
// [Session.ObjectInfo].
func ObjectInfos(ctx context.Context, rawURL string, oids []string,
	args ObjectInfoRequest, opts ...Option) ([]ObjectInfo, error) {
	s, err := Dial(ctx, rawURL, opts...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.ObjectInfo(ctx, oids, args)
}

// Exists reports whether a repository is reachable at rawURL.
//
// A successful [Dial] yields `(true, nil)` and the helper closes the
// resulting [Session] before returning. A [Dial] failure whose error
// chain matches [ErrNotFound] collapses to `(false, nil)` so a caller
// can distinguish "missing repository" from "transport blew up"
// without inspecting the error. Any other error propagates verbatim as
// `(false, err)`.
func Exists(ctx context.Context, rawURL string, opts ...Option) (bool, error) {
	s, err := Dial(ctx, rawURL, opts...)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	_ = s.Close()
	return true, nil
}

// DefaultBranch returns the canonical name of the branch HEAD points
// at on the remote — `refs/heads/main`, `refs/heads/master`, etc.
//
// On v2 the helper issues `ls-refs` with `ref-prefix HEAD`, `symrefs`,
// and `unborn`, then returns the [Ref.Symref] target attached to the
// `HEAD` entry. The `unborn` argument mirrors canonical Git's
// discovery client (`connect.c:591-592`). The wire layer silently
// ignores it when the server has not advertised `ls-refs=unborn`, so
// passing it here is always safe — older servers simply fall through
// to the v0/v1-style capability-list scan below. With `unborn`, a
// branch HEAD with no commits yet still surfaces as an unborn `HEAD`
// entry whose `symref-target:` carries the branch name; without it
// the server (`ls-refs.c:135-136`) would suppress HEAD entirely and
// the helper would fall through to [ErrNoDefaultBranch]. When the v2 server does
// not honour `symrefs` (or HEAD is detached and so carries no symref
// target) the helper falls back to the v0/v1-style capability scan
// over [Capabilities.Symrefs], because some servers surface the
// mapping there even on a v2 handshake.
//
// On v0/v1 the wire has no `ls-refs` equivalent and the symref
// information rides on the capability list; the helper scans
// [Capabilities.Symrefs] for an entry named `HEAD` and returns its
// target.
//
// When no symref target can be resolved DefaultBranch returns a
// [*ProtocolError] whose `Err` chains to [ErrNoDefaultBranch]. The
// surrounding [ProtocolError.Op] is `"ls-refs"` on a v2 server
// (the mapping is sought via the `ls-refs` command exchange) and
// `"advertisement"` on a v0/v1 server (the mapping is sought in
// the capability advertisement). Use `errors.Is(err,
// ErrNoDefaultBranch)` to detect the "repository present but HEAD
// has no symbolic target" condition (a detached HEAD on v2, or a
// v0/v1 server whose advertisement omits a `symref=HEAD:...`
// capability). A dial failure whose chain matches [ErrNotFound] means
// the repository itself is absent — the two sentinels are mutually
// exclusive.
func DefaultBranch(ctx context.Context, rawURL string, opts ...Option) (string, error) {
	s, err := Dial(ctx, rawURL, opts...)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.Close() }()

	caps := s.Capabilities()
	if caps.Version == ProtocolV2 {
		seq, err := s.Refs(ctx, RefsRequest{
			Prefixes: []string{"HEAD"},
			Symrefs:  true,
			Unborn:   true,
		})
		if err != nil {
			return "", err
		}
		for ref, err := range seq {
			if err != nil {
				return "", err
			}
			if ref.Name == "HEAD" && ref.Symref != "" {
				return ref.Symref, nil
			}
		}
		// Some servers do not honour the `symrefs` argument on v2 but
		// still expose the mapping via the capability list (rare, but
		// observed in older Git daemons). Fall through to the
		// capability scan rather than failing outright.
	}

	if target, ok := lookupHEADSymref(caps); ok {
		return target, nil
	}

	op := "ls-refs"
	if caps.Version != ProtocolV2 {
		op = "advertisement"
	}
	v := caps.Version
	return "", &ProtocolError{
		URL:     s.url,
		Op:      op,
		Version: &v,
		Server:  "HEAD has no symbolic target",
		Err:     ErrNoDefaultBranch,
	}
}

// Tags is a shorthand for [Refs] restricted to `refs/tags/` with
// `Peel` set so annotated tags carry their peeled commit id on
// [Ref.Peeled]. Tags do not carry symref targets, so [Ref.Symref] is
// always empty on every ref yielded by this iterator.
func Tags(ctx context.Context, rawURL string,
	opts ...Option) (iter.Seq2[Ref, error], error) {
	return Refs(ctx, rawURL, RefsRequest{
		Prefixes: []string{"refs/tags/"},
		Peel:     true,
	}, opts...)
}

// Heads is a shorthand for [Refs] restricted to `refs/heads/`.
func Heads(ctx context.Context, rawURL string,
	opts ...Option) (iter.Seq2[Ref, error], error) {
	return Refs(ctx, rawURL, RefsRequest{
		Prefixes: []string{"refs/heads/"},
	}, opts...)
}

// lookupHEADSymref scans caps.Symrefs for an entry named `HEAD` and
// returns its target. The v0/v1 path populates the slice from the
// capability advertisement; v2 leaves it empty in the common case but
// some servers still surface it, so the lookup is consulted as a
// fallback on v2 as well.
func lookupHEADSymref(caps Capabilities) (string, bool) {
	for _, sr := range caps.Symrefs {
		if sr.Name == "HEAD" {
			return sr.Target, true
		}
	}
	return "", false
}
