package server

import (
	"bytes"
	"testing"
)

// Test_maskAgent_replacesAgentCapValue pins the substantive contract:
// any pkt-line whose payload begins with `agent=` is rewritten to
// `agent=$AGENT$\n` and its 4-hex length prefix is recomputed. The
// agent value is the only byte region in the v2 capability
// advertisement that legitimately diverges between protocol-compliant
// `upload-pack` implementations (canonical Git emits
// `agent=git/<version>`; this library emits `agent=lsremote/0`).
func Test_maskAgent_replacesAgentCapValue(t *testing.T) {
	t.Parallel()

	// `version 2\n`        = 10 bytes → 0x000e
	// `agent=git/2.45.0\n` = 17 bytes → 0x0015
	// `ls-refs=unborn\n`   = 15 bytes → 0x0013
	// flush                → 0x0000
	in := []byte("000eversion 2\n0015agent=git/2.45.0\n0013ls-refs=unborn\n0000")
	// `agent=$AGENT$\n`    = 14 bytes → 0x0012
	want := []byte("000eversion 2\n0012agent=$AGENT$\n0013ls-refs=unborn\n0000")
	got := maskAgent(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("maskAgent mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// Test_maskAgent_passesThroughBytesWithNoAgentLine confirms the mask
// is a no-op on streams that do not contain an `agent=` line. The
// pkt-line framing must round-trip unchanged so the comparison
// harness does not flag a phantom divergence on the surrounding
// capability bytes.
func Test_maskAgent_passesThroughBytesWithNoAgentLine(t *testing.T) {
	t.Parallel()

	in := []byte("000eversion 2\n0013ls-refs=unborn\n0000")
	got := maskAgent(in)
	if !bytes.Equal(got, in) {
		t.Fatalf("maskAgent should be identity on no-agent input:\n got: %q\nwant: %q",
			got, in)
	}
}

// Test_maskAgent_idempotent locks the contract `MATRIX.md` advertises:
// `maskAgent(maskAgent(x)) == maskAgent(x)`. Applying the mask twice
// is harmless, so the harness can mask both sides without tracking
// whether one of them is already normalised.
func Test_maskAgent_idempotent(t *testing.T) {
	t.Parallel()

	in := []byte("000eversion 2\n0015agent=git/2.45.0\n0013ls-refs=unborn\n0000")
	once := maskAgent(in)
	twice := maskAgent(once)
	if !bytes.Equal(once, twice) {
		t.Fatalf("maskAgent not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
}

// Test_maskAgent_preservesControlPackets confirms the mask round-trips
// delim (`0001`) and response-end (`0002`) packets in addition to
// flush (`0000`). v2 command-request streams interleave delim and
// flush around the args section, and an unmasked mask that dropped
// delim packets would silently collapse the request grammar.
func Test_maskAgent_preservesControlPackets(t *testing.T) {
	t.Parallel()

	// `command=ls-refs\n` = 16 bytes → 0x0014
	// delim                → 0x0001
	// `peel\n`            =  5 bytes → 0x0009
	// flush                → 0x0000
	in := []byte("0014command=ls-refs\n00010009peel\n0000")
	got := maskAgent(in)
	if !bytes.Equal(got, in) {
		t.Fatalf("maskAgent should round-trip control packets unchanged:\n got: %q\nwant: %q",
			got, in)
	}
}

// Test_maskV2Advertisement_dropsCapsAndMasksAgent pins the
// per-advertisement mask: agent values are normalised (as maskAgent
// does) and additionally the data pkt-lines for the capabilities
// the two implementations legitimately diverge on are dropped from
// the stream. The remaining bytes — version line, framing, and the
// `agent` / `ls-refs` / `object-format` cap lines — must survive
// byte-identical so the harness asserts substantive equivalence on
// the common cap subset.
func Test_maskV2Advertisement_dropsCapsAndMasksAgent(t *testing.T) {
	t.Parallel()

	// Canonical-shape input: `version 2\n` + `agent=git/X\n` +
	// `ls-refs=unborn\n` + `fetch=shallow\n` + `server-option\n` +
	// `object-format=sha1\n` + flush.
	//
	// `version 2\n`           = 10 → 0x000e
	// `agent=git/X\n`         = 12 → 0x0010
	// `ls-refs=unborn\n`      = 15 → 0x0013
	// `fetch=shallow\n`       = 14 → 0x0012
	// `server-option\n`       = 14 → 0x0012
	// `object-format=sha1\n`  = 19 → 0x0017
	in := []byte(
		"000eversion 2\n" +
			"0010agent=git/X\n" +
			"0013ls-refs=unborn\n" +
			"0012fetch=shallow\n" +
			"0012server-option\n" +
			"0017object-format=sha1\n" +
			"0000",
	)
	// Want: agent substituted, fetch/server-option dropped, rest
	// preserved.
	// `agent=$AGENT$\n` = 14 → 0x0012
	want := []byte(
		"000eversion 2\n" +
			"0012agent=$AGENT$\n" +
			"0013ls-refs=unborn\n" +
			"0017object-format=sha1\n" +
			"0000",
	)
	got := maskV2Advertisement(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("maskV2Advertisement mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// Test_maskV2Advertisement_dropsObjectInfo confirms `object-info` is
// dropped from the our-side stream too. This emulator advertises
// `object-info` because it implements the command; canonical Git
// 2.54 emits it only under `feature.experimental` and the corpus is
// captured without that gate. Dropping the line from both sides
// (via the same idempotent mask) lets the harness still pass when
// either side carries it.
func Test_maskV2Advertisement_dropsObjectInfo(t *testing.T) {
	t.Parallel()

	// `version 2\n`         = 10 → 0x000e
	// `agent=lsremote/0\n`  = 17 → 0x0015
	// `ls-refs=unborn\n`    = 15 → 0x0013
	// `object-format=sha1\n` = 19 → 0x0017
	// `object-info\n`       = 12 → 0x0010
	in := []byte(
		"000eversion 2\n" +
			"0015agent=lsremote/0\n" +
			"0013ls-refs=unborn\n" +
			"0017object-format=sha1\n" +
			"0010object-info\n" +
			"0000",
	)
	// `agent=$AGENT$\n` = 14 → 0x0012
	want := []byte(
		"000eversion 2\n" +
			"0012agent=$AGENT$\n" +
			"0013ls-refs=unborn\n" +
			"0017object-format=sha1\n" +
			"0000",
	)
	got := maskV2Advertisement(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("maskV2Advertisement mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// Test_maskV2Advertisement_idempotent locks the documented contract.
// The harness applies the mask to both sides regardless of which
// one is canonical bytes off-disk and which is freshly-emitted
// from this package; idempotence makes that order-independent.
func Test_maskV2Advertisement_idempotent(t *testing.T) {
	t.Parallel()

	in := []byte(
		"000eversion 2\n" +
			"0010agent=git/X\n" +
			"0013ls-refs=unborn\n" +
			"0012fetch=shallow\n" +
			"0000",
	)
	once := maskV2Advertisement(in)
	twice := maskV2Advertisement(once)
	if !bytes.Equal(once, twice) {
		t.Fatalf("maskV2Advertisement not idempotent:\n once: %q\ntwice: %q",
			once, twice)
	}
}
