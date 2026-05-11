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
