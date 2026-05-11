package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openEmptyStore materialises the `empty` fixture from
// `testdata/repos/empty/` into a fresh `t.TempDir()` and opens a
// store rooted at it. The majority of tests in this package only
// need the empty repository, so the helper hides the
// fixture-name plumbing and the cleanup wiring at every call site.
func openEmptyStore(t *testing.T) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, "empty")
	s, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestServe_UnknownProtocolReturnsError pins the contract for any
// `PreferredProtocol` value outside the supported set: `Serve` must
// surface an error rather than emit silence.
func TestServe_UnknownProtocolReturnsError(t *testing.T) {
	store := openEmptyStore(t)

	clientToServer, _ := io.Pipe()
	pr, pw := io.Pipe()

	r := pktline.NewReader(clientToServer)
	w := pktline.NewWriter(pw)

	errCh := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), r, w, store, Options{
			Agent:             "test-agent/0.0",
			PreferredProtocol: transport.ProtocolVersion(99),
		})
		_ = pw.Close()
		errCh <- err
	}()

	// Drain the writer side so the goroutine's Close does not block
	// any in-flight write attempts, then assert the error.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()

	err := <-errCh
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"want errors.Is(err, ErrUnsupportedProtocol); got %v", err)
}

// TestServeCommandLoop_NoAdvertisementPrefix pins the smart-HTTP POST
// shape: [ServeCommandLoop] must not emit a `version 2\n` packet (or
// any advertisement byte) before the command response. The
// advertisement belongs to the GET probe; the POST body carries only
// the command response per canonical Git's
// `http-backend.c::service_rpc`. The test drives a single `ls-refs`
// request and asserts the first packet the server emits is a Data
// packet whose payload is not `version 2\n`.
func TestServeCommandLoop_NoAdvertisementPrefix(t *testing.T) {
	store := openEmptyStore(t)

	// Build a v2 request: `command=ls-refs`, delim, flush (end of
	// args), flush (empty request, terminates the loop).
	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)
	req.Write(flushBytes)

	var sink bytes.Buffer
	r := pktline.NewReader(bytes.NewReader(req.Bytes()))
	w := pktline.NewWriter(&sink)

	err := ServeCommandLoop(context.Background(), r, w, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})
	require.NoError(t, err)

	// The first packet must not be `version 2\n` — that is the
	// advertisement marker [Serve] would emit; [ServeCommandLoop]
	// must skip it entirely.
	pr := pktline.NewReader(bytes.NewReader(sink.Bytes()))
	pkt, err := pr.ReadPacket()
	require.NoError(t, err)
	assert.NotEqual(t, "version 2\n", string(pkt.Data),
		"ServeCommandLoop must not emit the v2 advertisement prefix; got %q",
		pkt.Data)
}

// TestServeCommandLoop_HandlesLSRefs drives a single `ls-refs` request
// through [ServeCommandLoop] and asserts that the response is exactly
// the ls-refs body — a single flush for the empty fixture — with no
// advertisement bytes preceding it. This is the same shape
// `runV2CommandLoop` is exercised against in
// [TestServe_V2SingleLSRefsThenEmpty], minus the leading
// advertisement that [Serve] would have emitted.
func TestServeCommandLoop_HandlesLSRefs(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(pktBytes("agent=test-client/0.0\n"))
	req.Write(delimBytes)
	req.Write(pktBytes("peel\n"))
	req.Write(flushBytes) // end of command-args
	req.Write(flushBytes) // empty-request: terminate session

	var sink bytes.Buffer
	r := pktline.NewReader(bytes.NewReader(req.Bytes()))
	w := pktline.NewWriter(&sink)

	err := ServeCommandLoop(context.Background(), r, w, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})
	require.NoError(t, err)
	assert.Equal(t, "0000", sink.String(),
		"ls-refs against empty fixture should emit a single flush")
}
