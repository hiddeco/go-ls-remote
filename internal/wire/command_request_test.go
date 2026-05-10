package wire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// TestEncodeV2CommandRequest_BodyShape pins the on-wire layout against
// the canonical v2 command-request grammar from `gitprotocol-v2.adoc`
// §"Command Request" and matched by `serve.c::process_request`:
//
//	command-request = command-line *capability-line delim-pkt *arg-line flush-pkt
//	command-line    = PKT-LINE("command=" cmd LF)
//	capability-line = PKT-LINE(cap LF)
//	arg-line        = PKT-LINE(arg LF)
//
// A subtle reorder (delim-before-cap, missing trailing LF, missing
// flush) would still parse on a lenient peer but diverge from the
// canonical grammar, so each packet is checked in turn.
func TestEncodeV2CommandRequest_BodyShape(t *testing.T) {
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, EncodeV2CommandRequest(w, "ls-refs",
		[]string{"peel", "symrefs"}, []string{"object-format=sha1"}))

	pr := pktline.NewReader(bytes.NewReader(buf.Bytes()))
	want := []struct {
		kind pktline.Kind
		data string
	}{
		{pktline.Data, "command=ls-refs\n"},
		{pktline.Data, "object-format=sha1\n"},
		{pktline.Delim, ""},
		{pktline.Data, "peel\n"},
		{pktline.Data, "symrefs\n"},
		{pktline.Flush, ""},
	}
	for i, w := range want {
		p, err := pr.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.Equal(t, w.kind, p.Kind, "packet %d kind", i)
		if w.kind == pktline.Data {
			assert.Equal(t, w.data, string(p.Data), "packet %d data", i)
		}
	}
}

// TestEncodeV2CommandRequest_NoCapsNoArgs covers the minimal shape: a
// command line, an empty capability section, the delim, an empty
// argument section, and the closing flush. Canonical Git's
// `serve.c::process_request` accepts this exact frame for a `command`
// that takes no caps/args.
func TestEncodeV2CommandRequest_NoCapsNoArgs(t *testing.T) {
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, EncodeV2CommandRequest(w, "ls-refs", nil, nil))

	pr := pktline.NewReader(bytes.NewReader(buf.Bytes()))
	want := []struct {
		kind pktline.Kind
		data string
	}{
		{pktline.Data, "command=ls-refs\n"},
		{pktline.Delim, ""},
		{pktline.Flush, ""},
	}
	for i, w := range want {
		p, err := pr.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.Equal(t, w.kind, p.Kind, "packet %d kind", i)
		if w.kind == pktline.Data {
			assert.Equal(t, w.data, string(p.Data), "packet %d data", i)
		}
	}
}

// TestEncodeV2CommandRequest_PropagatesWriterError pins that a writer
// failure mid-frame surfaces verbatim rather than being silently
// dropped. The encoder short-circuits on the first failed write so a
// caller can map the error onto its own protocol-level diagnostic.
func TestEncodeV2CommandRequest_PropagatesWriterError(t *testing.T) {
	bad := &errorWriter{err: io.ErrClosedPipe}
	bw := pktline.NewWriter(bad)
	err := EncodeV2CommandRequest(bw, "ls-refs", nil, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrClosedPipe),
		"writer error must propagate; got %v", err)
}

// TestEncodeV2CommandRequest_PropagatesWriterError_OnCap pins that an
// error during the capability loop also short-circuits and surfaces.
// A failed second WritePacket must not be masked by the trailing flush
// or any subsequent packet write.
func TestEncodeV2CommandRequest_PropagatesWriterError_OnCap(t *testing.T) {
	// Fail on the second write (capability line). The first WritePacket
	// (the command line) succeeds; the cap write hits the error.
	bad := &nthErrorWriter{n: 2, err: io.ErrShortWrite}
	bw := pktline.NewWriter(bad)
	err := EncodeV2CommandRequest(bw, "ls-refs", nil, []string{"object-format=sha1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrShortWrite),
		"capability-loop write error must propagate; got %v", err)
}

// TestEncodeV2CommandRequest_PropagatesWriterError_OnArg pins error
// propagation during the argument loop, after the delim.
func TestEncodeV2CommandRequest_PropagatesWriterError_OnArg(t *testing.T) {
	// Three successful writes (command, cap, delim) then fail on the
	// argument line.
	bad := &nthErrorWriter{n: 4, err: io.ErrShortWrite}
	bw := pktline.NewWriter(bad)
	err := EncodeV2CommandRequest(bw, "ls-refs",
		[]string{"peel"}, []string{"object-format=sha1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrShortWrite),
		"argument-loop write error must propagate; got %v", err)
}

// TestValidateV2CommandPayloads_Accepts checks that a payload exactly at
// the [pktline.MaxPayload] boundary is accepted: the framing is
// `command=` + name + LF for the command line, and value + LF for cap
// and arg lines, so the largest accepted name is `MaxPayload-len("command=")-1`.
func TestValidateV2CommandPayloads_Accepts(t *testing.T) {
	largestName := strings.Repeat("a", pktline.MaxPayload-len("command=")-1)
	largestValue := strings.Repeat("a", pktline.MaxPayload-1)

	require.NoError(t, ValidateV2CommandPayloads(largestName, nil, nil))
	require.NoError(t, ValidateV2CommandPayloads("ls-refs",
		[]string{largestValue}, nil))
	require.NoError(t, ValidateV2CommandPayloads("ls-refs",
		nil, []string{largestValue}))
}

// TestValidateV2CommandPayloads_RejectsOversize covers each axis of
// the validator: command name, capability, and argument. Every error
// must wrap [pktline.ErrPayloadTooLarge] for [errors.Is] matching.
func TestValidateV2CommandPayloads_RejectsOversize(t *testing.T) {
	overlong := strings.Repeat("a", pktline.MaxPayload)
	tests := []struct {
		name string
		cmd  string
		args []string
		caps []string
	}{
		{name: "oversize command name", cmd: overlong},
		{name: "oversize capability", cmd: "ls-refs", caps: []string{overlong}},
		{name: "oversize argument", cmd: "ls-refs", args: []string{overlong}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateV2CommandPayloads(tc.cmd, tc.args, tc.caps)
			require.Error(t, err)
			assert.True(t, errors.Is(err, pktline.ErrPayloadTooLarge),
				"oversize input must wrap pktline.ErrPayloadTooLarge; got %v", err)
		})
	}
}

// errorWriter is an [io.Writer] that always fails with err. Minimal
// stub for the encoder's first-write error-propagation contract.
type errorWriter struct{ err error }

func (e *errorWriter) Write(_ []byte) (int, error) { return 0, e.err }

// nthErrorWriter accepts the first n-1 writes and fails the n-th.
// Each [pktline.Writer] packet emits exactly one underlying Write call
// (the writer assembles header+payload in a single buffer), so n maps
// directly to "fail on the n-th packet".
type nthErrorWriter struct {
	n   int
	err error
	i   int
}

func (e *nthErrorWriter) Write(p []byte) (int, error) {
	e.i++
	if e.i >= e.n {
		return 0, e.err
	}
	return len(p), nil
}
