package lsremote

import (
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/wire"
)

// Benchmark_convertCaps_v2Typical pins the cost of `convertCaps` for a
// typical v2 capability advertisement. `convertCaps` runs once per
// `Dial` call: it allocates the `Raw` map, the `Commands` slice, and the
// `LSRefsArgs` / `ObjectInfoArgs` slices from a wire-layer
// `RawCapabilities`. The v2 path also intersects the advertised names
// against the curated command set via `slices.Contains`, which touches
// every advertised capability name.
//
// The fixture models a realistic v2 server: `agent`, `object-format=sha1`,
// `ls-refs=unborn`, `fetch=shallow filter`, `object-info=size`,
// `server-option`, `session-id`, and `bundle-uri` — eight capabilities,
// four of which are recognized commands. This is the cost the average
// caller pays on every `Dial`.
//
// Inputs are constructed outside the timed loop. The loop measures only
// the `convertCaps` body, including all allocations it performs. A
// future refactor that avoids one allocation per `Raw`-map entry would
// lower both wall-time and allocs/op here.
func Benchmark_convertCaps_v2Typical(b *testing.B) {
	raw := wire.RawCapabilities{
		{Name: "agent", Value: "git/2.44.0"},
		{Name: "object-format", Value: "sha1"},
		{Name: "ls-refs", Value: "unborn"},
		{Name: "fetch", Value: "shallow filter"},
		{Name: "object-info", Value: "size"},
		{Name: "server-option"},
		{Name: "session-id", Value: "abc123def456"},
		{Name: "bundle-uri"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = convertCaps(raw, ProtocolV2)
	}
}

// Benchmark_convertCaps_v0WithSymrefs pins the cost of `convertCaps` for
// a v0 capability advertisement that carries `symref=` entries. The v0
// path follows a different branch than v2: it skips the command-name
// intersection but walks every `symref=NAME:TARGET` token through
// `strings.Cut` to populate `Capabilities.Symrefs`. The B4 normalise
// pass also fires here, setting `ObjectFormat` to `ObjectFormatSHA1`
// when the server omits `object-format`.
//
// The fixture models a server with `agent`, an explicit `object-format`,
// and three `symref` entries (HEAD plus two remote-tracking heads) —
// a shape typical of a multi-remote client configuration. The hot region
// is the symref-slice population and the `ObjectFormatSHA1` default
// branch from the B4 normalise pass.
//
// Inputs are constructed outside the timed loop. A corpus generator that
// produces v0 advertisements from real `git upload-pack` banner bytes
// would close the gap between this synthetic fixture and production
// traffic; the banner bytes live in `git upload-pack`'s initial pkt-line
// flush per [gitprotocol-pack.adoc §"reference-discovery"].
//
// [gitprotocol-pack.adoc §"reference-discovery"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#reference-discovery
func Benchmark_convertCaps_v0WithSymrefs(b *testing.B) {
	raw := wire.RawCapabilities{
		{Name: "agent", Value: "git/2.44.0"},
		{Name: "object-format", Value: "sha1"},
		{Name: "symref", Value: "HEAD:refs/heads/main"},
		{Name: "symref", Value: "refs/remotes/origin/HEAD:refs/heads/main"},
		{Name: "symref", Value: "refs/remotes/upstream/HEAD:refs/heads/develop"},
		{Name: "multi_ack"},
		{Name: "side-band-64k"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = convertCaps(raw, ProtocolV0)
	}
}
