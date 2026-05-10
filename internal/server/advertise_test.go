package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pktLine encodes payload as a single pkt-line, prefixing it with a
// 4-hex-digit length field that includes the prefix itself. The helper
// keeps the byte-pinned expectations in this file readable.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", 4+len(payload), payload)
}

// runAdvertise runs [Serve] synchronously against the given store and
// options, returning the bytes it emitted. The client-to-server side
// is a [bytes.Reader] preloaded with a single flush packet — the v2
// empty-request terminator from `serve.c::process_request` lines
// 314-321 — so the v2 command loop exits cleanly. The v0 path returns
// before reading any client byte, so the preloaded flush is harmless
// there too.
func runAdvertise[H objfmt.HashType](t *testing.T, store *objstore.Store[H], opts Options) []byte {
	t.Helper()

	src := bytes.NewReader([]byte("0000"))
	var sink bytes.Buffer

	r := pktline.NewReader(src)
	w := pktline.NewWriter(&sink)

	require.NoError(t, Serve(context.Background(), r, w, store, opts))
	return sink.Bytes()
}

// TestServe_V2AdvertisementBytes pins the full v2 capability
// advertisement against the `empty` fixture (sha1) with an explicit
// agent string. The byte-pinned literal exercises the canonical
// emission order (`agent`, `ls-refs`, `object-format`, `object-info`)
// from `serve.c::protocol_v2_advertise_capabilities` and the pkt-line
// framing from `pkt-line.c::packet_write`.
func TestServe_V2AdvertisementBytes(t *testing.T) {
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})

	want := pktLine("version 2\n") +
		pktLine("agent=test-agent/0.0\n") +
		pktLine("ls-refs=unborn\n") +
		pktLine("object-format=sha1\n") +
		pktLine("object-info\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestServe_V2AdvertisementDefaultsAgent pins the fallback when
// [Options.Agent] is empty: the server advertises
// [wire.DefaultUserAgent], matching `serve.c::agent_advertise`
// lines 25-31 (canonical Git falls back to `git_user_agent_sanitized`).
func TestServe_V2AdvertisementDefaultsAgent(t *testing.T) {
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		PreferredProtocol: transport.ProtocolV2,
	})

	wantAgent := pktLine("agent=" + wire.DefaultUserAgent + "\n")
	assert.Contains(t, string(got), wantAgent,
		"want agent line %q in advertisement", wantAgent)
}

// TestServe_V2AdvertisementSHA256 pins the `object-format` line for a
// sha256 repository, matching `serve.c::object_format_advertise`
// lines 53-58 (canonical Git emits the repository's
// `the_hash_algo->name`).
func TestServe_V2AdvertisementSHA256(t *testing.T) {
	gitdir := testfixture.MaterializeRepo(t, "sha256")
	store, err := objstore.Open[objfmt.SHA256Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})

	want := pktLine("version 2\n") +
		pktLine("agent=test-agent/0.0\n") +
		pktLine("ls-refs=unborn\n") +
		pktLine("object-format=sha256\n") +
		pktLine("object-info\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestWriteV0Advertisement_AllocsPerRef pins the per-ref allocation
// budget for `writeV0Advertisement`'s ref-emission loop. After the
// scratch-buffer reuse and `objfmt.Hash.AppendHex` migration the loop
// body has no per-ref hex-string allocation and writes OID hex
// directly into the reused `[]byte` scratch. The budget is set loose
// enough not to flake on an off-by-one ref-count rounding (a small
// per-call constant amortised across 1000 refs) but tight enough to
// fail against either the pre-scratch shape or the post-scratch /
// pre-AppendHex two-alloc shape.
//
// The fixture carries 1000 packed refs (with a peel mix on one arm)
// to amortise the HEAD line and the cap-list assembly so the per-ref
// average isolates the loop body's allocation cost.
func TestWriteV0Advertisement_AllocsPerRef(t *testing.T) {
	const refCount = 1000
	const maxAllocsPerRef = 1.0

	for _, tc := range []struct {
		name     string
		withPeel bool
	}{
		{name: "no-peel", withPeel: false},
		{name: "with-peel", withPeel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := buildBenchPackedRefsRepo(t, refCount, tc.withPeel)
			opts := Options{
				Agent:             "test-agent/0.0",
				PreferredProtocol: transport.ProtocolV0,
			}
			w := pktline.NewWriter(io.Discard)

			avg := testing.AllocsPerRun(20, func() {
				if err := writeV0Advertisement(w, store, opts); err != nil {
					t.Fatal(err)
				}
			})

			perRef := avg / float64(refCount)
			if perRef > maxAllocsPerRef {
				t.Fatalf("post-fix allocs/ref = %.2f (total %.0f / %d refs), want <= %.1f",
					perRef, avg, refCount, maxAllocsPerRef)
			}
		})
	}
}
