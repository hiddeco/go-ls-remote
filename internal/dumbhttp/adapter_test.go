package dumbhttp_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/dumbhttp"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/transport"
)

// SHA-1 hashes from the canonical example in
// `gitprotocol-http.adoc` lines 172-175.
const (
	oidMaint  = "95dcfa3633004da0049d3d0fa03f80589cbcaf31"
	oidMaster = "d049f6c27a2244e12041955e262a404c7faba355"
	oidV1Tag  = "2cb58b79488a98d2721cea644875a8dd0026b115"
	oidV1Peel = "a3c2e2402b99163d1d59756e5f207ae21cccba4c"
)

func TestNewAdapter_TypicalRefs(t *testing.T) {
	body := "" +
		oidMaint + "\trefs/heads/maint\n" +
		oidMaster + "\trefs/heads/master\n" +
		oidV1Tag + "\trefs/tags/v1.0\n" +
		oidV1Peel + "\trefs/tags/v1.0^{}\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))
	ad, err := wire.ParseAdvertisement(pr, nil)
	require.NoError(t, err)

	assert.Equal(t, transport.ProtocolV0, ad.Version)
	assert.Empty(t, ad.Caps)
	require.Len(t, ad.Refs, 3)
	assert.Equal(t, wire.RawRef{OID: oidMaint, Name: "refs/heads/maint"}, ad.Refs[0])
	assert.Equal(t, wire.RawRef{OID: oidMaster, Name: "refs/heads/master"}, ad.Refs[1])
	assert.Equal(t,
		wire.RawRef{OID: oidV1Tag, Name: "refs/tags/v1.0", Peeled: oidV1Peel},
		ad.Refs[2])
}

func TestNewAdapter_EmptyBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace only", "\n\n   \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := dumbhttp.NewAdapter(strings.NewReader(tc.body))
			ad, err := wire.ParseAdvertisement(pr, nil)
			require.NoError(t, err)

			assert.Equal(t, transport.ProtocolV0, ad.Version)
			assert.Empty(t, ad.Refs)
			assert.Empty(t, ad.Caps)
		})
	}
}

func TestNewAdapter_PeeledTagOnFirstRef(t *testing.T) {
	// First ref is an annotated tag whose peel line follows immediately.
	// The adapter must emit the main line with the NUL/no-cap marker,
	// then the peel as a subsequent ref pkt-line.
	body := "" +
		oidV1Tag + "\trefs/tags/v1.0\n" +
		oidV1Peel + "\trefs/tags/v1.0^{}\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))
	ad, err := wire.ParseAdvertisement(pr, nil)
	require.NoError(t, err)

	require.Len(t, ad.Refs, 1)
	assert.Equal(t,
		wire.RawRef{OID: oidV1Tag, Name: "refs/tags/v1.0", Peeled: oidV1Peel},
		ad.Refs[0])
}

func TestNewAdapter_SpaceSeparatedTolerance(t *testing.T) {
	// Some real-world dumb servers use a single space rather than the
	// HTAB the spec mandates. The adapter falls back to whitespace
	// splitting so the wire layer still receives a valid v0 stream.
	body := "" +
		oidMaint + " refs/heads/maint\n" +
		oidMaster + " refs/heads/master\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))
	ad, err := wire.ParseAdvertisement(pr, nil)
	require.NoError(t, err)

	require.Len(t, ad.Refs, 2)
	assert.Equal(t, wire.RawRef{OID: oidMaint, Name: "refs/heads/maint"}, ad.Refs[0])
	assert.Equal(t, wire.RawRef{OID: oidMaster, Name: "refs/heads/master"}, ad.Refs[1])
}

func TestNewAdapter_MalformedLine(t *testing.T) {
	// A line with only an OID (no refname) is malformed. Reading the
	// synthesised pkt-line stream must surface an error wrapping
	// [dumbhttp.ErrMalformedRefLine].
	body := oidMaint + "\trefs/heads/maint\n" + oidMaster + "\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))

	var sawErr error
	for i := 0; i < 16 && sawErr == nil; i++ {
		_, err := pr.ReadPacket()
		if err != nil {
			sawErr = err
			break
		}
	}
	require.Error(t, sawErr)
	assert.True(t, errors.Is(sawErr, dumbhttp.ErrMalformedRefLine),
		"expected error wrapping ErrMalformedRefLine, got %v", sawErr)
}

func TestNewAdapter_BlankAndCommentLines(t *testing.T) {
	body := "" +
		"\n" +
		"# a server-side comment\n" +
		oidMaint + "\trefs/heads/maint\n" +
		"\n" +
		"# another comment\n" +
		oidMaster + "\trefs/heads/master\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))
	ad, err := wire.ParseAdvertisement(pr, nil)
	require.NoError(t, err)

	require.Len(t, ad.Refs, 2)
	assert.Equal(t, wire.RawRef{OID: oidMaint, Name: "refs/heads/maint"}, ad.Refs[0])
	assert.Equal(t, wire.RawRef{OID: oidMaster, Name: "refs/heads/master"}, ad.Refs[1])
}

func TestNewAdapter_TrailingCR(t *testing.T) {
	// CRLF line endings — common from servers running on Windows or
	// after a charset conversion. Trailing CR must be trimmed.
	body := "" +
		oidMaint + "\trefs/heads/maint\r\n" +
		oidMaster + "\trefs/heads/master\r\n"

	pr := dumbhttp.NewAdapter(strings.NewReader(body))
	ad, err := wire.ParseAdvertisement(pr, nil)
	require.NoError(t, err)

	require.Len(t, ad.Refs, 2)
	assert.Equal(t, wire.RawRef{OID: oidMaint, Name: "refs/heads/maint"}, ad.Refs[0])
	assert.Equal(t, wire.RawRef{OID: oidMaster, Name: "refs/heads/master"}, ad.Refs[1])
}

// errSentinel is the canary error injected by errReader to verify mid-
// stream read errors propagate through the adapter.
var errSentinel = errors.New("dumbhttp test sentinel")

// errReader emits prefix bytes once, then errSentinel on every
// subsequent Read. It models a transport that fails partway through
// streaming the dumb info/refs body.
type errReader struct {
	prefix []byte
	done   bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.done {
		n := copy(p, e.prefix)
		e.prefix = e.prefix[n:]
		if len(e.prefix) == 0 {
			e.done = true
		}
		return n, nil
	}
	return 0, errSentinel
}

func TestNewAdapter_ReadErrorPropagation(t *testing.T) {
	// One complete ref line followed by a hard read error. After the
	// first synthesized pkt-line is consumed, the next ReadPacket call
	// must surface errSentinel via errors.Is.
	src := &errReader{prefix: []byte(oidMaint + "\trefs/heads/maint\n")}
	pr := dumbhttp.NewAdapter(src)

	var lastErr error
	for i := 0; i < 16; i++ {
		_, err := pr.ReadPacket()
		if err != nil {
			lastErr = err
			break
		}
	}
	require.Error(t, lastErr)
	assert.True(t, errors.Is(lastErr, errSentinel),
		"expected error wrapping errSentinel, got %v", lastErr)
	// Sanity: not io.EOF — we want the underlying transport error, not
	// a clean stream end.
	assert.False(t, errors.Is(lastErr, io.EOF))
}
