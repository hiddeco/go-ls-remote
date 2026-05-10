package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runV2Session runs a single in-process [Serve] invocation against
// the given store, feeding request as the client-to-server byte
// stream. It returns the bytes the server emitted after the
// advertisement (with the advertisement bytes stripped) and the error
// [Serve] returned.
//
// The client side is modelled as a [bytes.Reader]: the server sees
// the request bytes and then a clean EOF, mirroring the real-world
// case where the client closes its writer after the last request.
// There is no need for an [io.Pipe] — the request is fully known up
// front and the server reads packets one at a time, so a buffered
// reader serialises identically to the live wire.
func runV2Session(t *testing.T, store *objstore.Store, request []byte) (response []byte, err error) {
	t.Helper()

	src := bytes.NewReader(request)
	var sink bytes.Buffer
	r := pktline.NewReader(src)
	w := pktline.NewWriter(&sink)

	err = Serve(context.Background(), r, w, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})

	advLen := lenV2Advertisement(t, store, "test-agent/0.0")
	all := sink.Bytes()
	require.GreaterOrEqual(t, len(all), advLen,
		"server emitted fewer bytes than the expected advertisement length")
	return all[advLen:], err
}

// lenV2Advertisement returns the byte length of the v2 advertisement
// the server emits for the given store and agent. The number is
// deterministic from the fixture and the cap set in
// [writeV2Advertisement].
func lenV2Advertisement(t *testing.T, store *objstore.Store, agent string) int {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, writeV2Advertisement(w, store, Options{
		Agent:             agent,
		PreferredProtocol: transport.ProtocolV2,
	}))
	return buf.Len()
}

// pktBytes encodes payload as a pkt-line with the on-wire length
// prefix. The framing layer treats the trailing LF (if any) as part of
// the payload; the helper does not add or strip it.
func pktBytes(payload string) []byte {
	hdr := make([]byte, 4)
	const hex = "0123456789abcdef"
	v := 4 + len(payload)
	hdr[0] = hex[(v>>12)&0xf]
	hdr[1] = hex[(v>>8)&0xf]
	hdr[2] = hex[(v>>4)&0xf]
	hdr[3] = hex[v&0xf]
	return append(hdr, payload...)
}

// flushBytes is the on-wire flush packet.
var flushBytes = []byte("0000")

// delimBytes is the on-wire delim packet.
var delimBytes = []byte("0001")

// TestServe_V2EmptyRequestTerminates verifies the canonical
// empty-request termination path from `serve.c::process_request`
// lines 314-321: a flush received before any `command=` or capability
// line means the client wants to terminate the session, and the loop
// returns cleanly.
func TestServe_V2EmptyRequestTerminates(t *testing.T) {
	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, flushBytes)
	require.NoError(t, err)
	assert.Empty(t, resp,
		"empty request should not elicit any post-advertisement bytes; got %q", resp)
}

// TestServe_V2StreamCloseTerminates verifies the EOF-before-request
// path from `serve.c::process_request` lines 292-297: the canonical
// peek returns `PACKET_READ_EOF` and the loop exits with status 1.
// In our emulator the same situation surfaces as `io.EOF` from the
// reader; `Serve` must return nil.
func TestServe_V2StreamCloseTerminates(t *testing.T) {
	store := openEmptyStore(t)

	// An empty request body — the server reads one packet, gets EOF,
	// and terminates cleanly.
	resp, err := runV2Session(t, store, nil)
	require.NoError(t, err)
	assert.Empty(t, resp)
}

// TestServe_V2SingleLSRefsThenEmpty drives a single `ls-refs` request
// followed by a bare flush. The stub handler must drain its args
// section up to the terminating flush and emit a single flush as its
// response; the loop must then read the empty-request flush and exit
// cleanly.
func TestServe_V2SingleLSRefsThenEmpty(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(pktBytes("agent=test-client/0.0\n"))
	req.Write(delimBytes)
	req.Write(pktBytes("peel\n"))
	req.Write(flushBytes) // end of command-args
	req.Write(flushBytes) // empty-request: terminate session

	resp, err := runV2Session(t, store, req.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp),
		"ls-refs stub should emit a single flush")
}

// TestServe_V2SequenceLSRefsObjectInfo drives two commands per session:
// `ls-refs`, then `object-info`, then a bare flush. Both stubs must
// run; the response is two flushes concatenated, one per stub.
func TestServe_V2SequenceLSRefsObjectInfo(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)
	req.Write(pktBytes("command=object-info\n"))
	req.Write(delimBytes)
	req.Write(pktBytes("size\n"))
	req.Write(flushBytes)
	req.Write(flushBytes) // empty-request

	resp, err := runV2Session(t, store, req.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "00000000", string(resp),
		"ls-refs + object-info stubs should emit two flushes")
}

// TestServe_V2UnknownCommand drives a request with an unrecognised
// `command=<name>`. The server emits a structured `ERR command not
// supported` data pkt-line followed by a flush, and `Serve` returns
// an error wrapping [wire.ErrServerRefused].
func TestServe_V2UnknownCommand(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=fetch\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)

	resp, err := runV2Session(t, store, req.Bytes())
	require.Error(t, err)
	assert.True(t, errors.Is(err, wire.ErrServerRefused),
		"want errors.Is(err, wire.ErrServerRefused); got %v", err)

	r := pktline.NewReader(bytes.NewReader(resp))
	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, pkt.Kind)
	assert.True(t, strings.HasPrefix(string(pkt.Data), "ERR "),
		"want ERR-prefix; got %q", pkt.Data)
	assert.Equal(t, "ERR command not supported\n", string(pkt.Data))

	pkt, err = r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, pkt.Kind)
}

// TestServe_V2FlushBeforeDelimDispatchesWithEmptyArgs pins the
// canonical "flush instead of delim" path from
// `serve.c::process_request` lines 314-329: the dispatcher detects a
// flush in place of the args-section delim, leaves the flush on the
// wire ("the flush packet isn't consume here"), and dispatches the
// command. The command's handler then reads the flush as its own
// args-section terminator and emits an empty body.
//
// The follow-up `command=object-info` request on the same stream is
// the canary: if the dispatcher (incorrectly) consumed the early
// flush, the ls-refs handler's args-section reader would see the
// `command=object-info\n` line and refuse it as an unknown argument.
// A clean end-to-end response with both bodies emitted in sequence
// proves the early flush stayed on the wire and was consumed by the
// ls-refs handler exactly once.
func TestServe_V2FlushBeforeDelimDispatchesWithEmptyArgs(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(flushBytes) // early flush — args terminator, NOT consumed by outer loop
	req.Write(pktBytes("command=object-info\n"))
	req.Write(delimBytes)
	req.Write(pktBytes("size\n"))
	req.Write(flushBytes) // end of object-info args
	req.Write(flushBytes) // empty-request: terminate session

	resp, err := runV2Session(t, store, req.Bytes())
	require.NoError(t, err)

	// ls-refs body for an empty repo with no args: just a flush.
	// object-info body for an empty OID list (even with `size`):
	// just a flush, per `protocol-caps.c:44-45`.
	assert.Equal(t, "00000000", string(resp))
}
