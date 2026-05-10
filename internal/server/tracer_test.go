package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTracer captures every [trace.Event] passed through
// [trace.Tracer.OnEvent]. The slice preserves emission order so tests
// can assert on the start/end pairing of [trace.CommandEvent]s. The
// concrete type is local to this test file so the server package's
// public API is not pulled into the test contract.
type recordingTracer struct {
	events []trace.Event
}

func (r *recordingTracer) OnEvent(e trace.Event) {
	r.events = append(r.events, e)
}

// commandEvents filters the captured events to just the
// [trace.CommandEvent] entries, preserving order. The dispatcher emits
// CommandEvents and nothing else for the cases under test, but a future
// change might fan in additional event types — filtering keeps the
// assertions local to the slice they care about.
func (r *recordingTracer) commandEvents() []trace.CommandEvent {
	var out []trace.CommandEvent
	for _, ev := range r.events {
		if ce, ok := ev.(trace.CommandEvent); ok {
			out = append(out, ce)
		}
	}
	return out
}

// runV2SessionWithTracer mirrors [runV2Session] but threads tracer
// through [Options.Tracer] so the dispatcher's emissions are captured.
// The advertisement bytes are stripped from the returned response, in
// the same way `runV2Session` does, to keep the assertions focused on
// the post-advertisement bytes.
func runV2SessionWithTracer(t *testing.T, store *objstore.Store[objfmt.SHA1Hash],
	tracer trace.Tracer, request []byte) (response []byte, err error) {
	t.Helper()

	src := bytes.NewReader(request)
	var sink bytes.Buffer
	r := pktline.NewReader(src)
	w := pktline.NewWriter(&sink)

	err = Serve(context.Background(), r, w, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
		Tracer:            tracer,
	})

	advLen := lenV2Advertisement(t, store, "test-agent/0.0")
	all := sink.Bytes()
	require.GreaterOrEqual(t, len(all), advLen,
		"server emitted fewer bytes than the expected advertisement length")
	return all[advLen:], err
}

// TestServe_TracerSingleLSRefs verifies the dispatcher emits exactly
// one start/end pair for a single `ls-refs` command. The start event
// has zero `Duration` and a nil `Err`; the end event has a positive
// `Duration` and a nil `Err` because the handler succeeded. Both
// events have an empty `URL` (the in-process emulator has no remote
// URL) and the canonical command name.
func TestServe_TracerSingleLSRefs(t *testing.T) {
	store := openEmptyStore(t)
	tr := &recordingTracer{}

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	req.Write(flushBytes) // end of command-args
	req.Write(flushBytes) // empty-request: terminate session

	_, err := runV2SessionWithTracer(t, store, tr, req.Bytes())
	require.NoError(t, err)

	events := tr.commandEvents()
	require.Len(t, events, 2,
		"expected exactly one start/end pair for a single ls-refs dispatch")

	start := events[0]
	end := events[1]

	assert.Equal(t, "ls-refs", start.Name)
	assert.Equal(t, trace.CommandStart, start.Phase)
	assert.Empty(t, start.URL,
		"in-process emulator has no remote URL; URL must be empty")
	assert.Equal(t, time.Duration(0), start.Duration,
		"start event must carry a zero duration")
	assert.NoError(t, start.Err,
		"start event must carry a nil error")
	assert.False(t, start.Time.IsZero(),
		"start event must carry a non-zero wall-clock time")

	assert.Equal(t, "ls-refs", end.Name)
	assert.Equal(t, trace.CommandEnd, end.Phase)
	assert.Empty(t, end.URL)
	assert.Greater(t, int64(end.Duration), int64(0),
		"end event must carry a positive duration")
	assert.NoError(t, end.Err,
		"successful handler must surface a nil error on the end event")
	assert.False(t, end.Time.IsZero())
	assert.False(t, end.Time.Before(start.Time),
		"end time must not precede start time")
}

// TestServe_TracerSequenceLSRefsObjectInfo drives `ls-refs` followed by
// `object-info` in one session and asserts the dispatcher emitted four
// events in the expected order: ls-refs Start, ls-refs End,
// object-info Start, object-info End.
func TestServe_TracerSequenceLSRefsObjectInfo(t *testing.T) {
	store := openEmptyStore(t)
	tr := &recordingTracer{}

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)
	req.Write(pktBytes("command=object-info\n"))
	req.Write(delimBytes)
	req.Write(pktBytes("size\n"))
	req.Write(flushBytes)
	req.Write(flushBytes) // empty-request

	_, err := runV2SessionWithTracer(t, store, tr, req.Bytes())
	require.NoError(t, err)

	events := tr.commandEvents()
	require.Len(t, events, 4,
		"expected two start/end pairs for ls-refs then object-info")

	assert.Equal(t, "ls-refs", events[0].Name)
	assert.Equal(t, trace.CommandStart, events[0].Phase)
	assert.Equal(t, "ls-refs", events[1].Name)
	assert.Equal(t, trace.CommandEnd, events[1].Phase)
	assert.NoError(t, events[1].Err)

	assert.Equal(t, "object-info", events[2].Name)
	assert.Equal(t, trace.CommandStart, events[2].Phase)
	assert.Equal(t, "object-info", events[3].Name)
	assert.Equal(t, trace.CommandEnd, events[3].Phase)
	assert.NoError(t, events[3].Err)
}

// TestServe_TracerObjectInfoCorrupt drives the corrupt-pack scenario
// from [TestObjectInfo_CorruptObject] and asserts the captured
// [trace.CommandEvent] for the `CommandEnd` phase carries a non-nil
// `Err` wrapping [wire.ErrServerRefused], so a tracer-only consumer can
// observe the protocol-level refusal without re-decoding the response
// stream.
func TestServe_TracerObjectInfoCorrupt(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "repos", "pack-only")
	require.NoError(t, copyFixtureTree(src, dir))
	require.NoError(t, os.Rename(
		filepath.Join(dir, "dotgit"),
		filepath.Join(dir, ".git")))

	packPath := filepath.Join(dir, ".git", "objects", "pack", "three-objects.pack")
	flipPackByte(t, packPath, 64)

	store, err := objstore.Open[objfmt.SHA1Hash](filepath.Join(dir, ".git"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	tr := &recordingTracer{}
	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + packCommitOID + "\n",
	})
	_, serveErr := runV2SessionWithTracer(t, store, tr, req)
	require.Error(t, serveErr)
	require.True(t, errors.Is(serveErr, wire.ErrServerRefused),
		"want errors.Is(err, wire.ErrServerRefused); got %v", serveErr)

	events := tr.commandEvents()
	require.Len(t, events, 2,
		"corrupt-object dispatch still emits start/end exactly once")

	start := events[0]
	end := events[1]
	assert.Equal(t, "object-info", start.Name)
	assert.Equal(t, trace.CommandStart, start.Phase)
	assert.NoError(t, start.Err)

	assert.Equal(t, "object-info", end.Name)
	assert.Equal(t, trace.CommandEnd, end.Phase)
	require.Error(t, end.Err)
	assert.True(t, errors.Is(end.Err, wire.ErrServerRefused),
		"end-event Err must wrap wire.ErrServerRefused; got %v", end.Err)
}

// TestServe_TracerNilNoOp verifies the documented contract: a nil
// [Options.Tracer] disables emission entirely and the dispatch path
// runs without panic. The [trace.Emit] helper is itself nil-safe; this
// test exercises the call sites that wrap each handler dispatch.
func TestServe_TracerNilNoOp(t *testing.T) {
	store := openEmptyStore(t)

	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)
	req.Write(flushBytes) // empty-request

	require.NotPanics(t, func() {
		_, err := runV2SessionWithTracer(t, store, nil, req.Bytes())
		require.NoError(t, err)
	})
}

// TestServe_TracerUnknownCommandSilent pins that the unknown-command
// path emits NO [trace.CommandEvent]. The canonical-Git-equivalent
// operation here is the structured ERR refusal, which is more naturally
// surfaced via the caller-visible error than via a tracer event;
// keeping the unknown-command path silent on the tracer keeps the
// emitted events focused on real command dispatches.
func TestServe_TracerUnknownCommandSilent(t *testing.T) {
	store := openEmptyStore(t)
	tr := &recordingTracer{}

	var req bytes.Buffer
	req.Write(pktBytes("command=fetch\n"))
	req.Write(delimBytes)
	req.Write(flushBytes)

	_, err := runV2SessionWithTracer(t, store, tr, req.Bytes())
	require.Error(t, err)
	require.True(t, errors.Is(err, wire.ErrServerRefused))

	events := tr.commandEvents()
	assert.Empty(t, events,
		"unknown-command path must not emit any CommandEvent; got %d", len(events))
}

// copyFixtureTree and flipPackByte live in object_info_test.go.
