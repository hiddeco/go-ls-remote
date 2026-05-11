package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// FuzzWriteStreamRequest exercises [WriteStreamRequest] with arbitrary
// [transport.URL] shapes and [transport.ProtocolVersion] selections.
// The contract under test is twofold: the encoder is total — it never
// panics for any input — and a successful write produces a frameable
// pkt-line stream that round-trips through [pktline.Reader] without a
// framing error. The host-parameter logic at `connect.c:1288-1298`
// rebrackets IPv6 literals when a port is present, so seeds cover the
// IPv4/IPv6 with-port and bare-host variants alongside the four
// [transport.ProtocolVersion] selectors (nil and the three concrete
// versions).
func FuzzWriteStreamRequest(f *testing.F) {
	for _, seed := range writeStreamRequestFuzzSeeds() {
		f.Add(seed.scheme, seed.host, seed.port, seed.path, seed.vSel)
	}

	f.Fuzz(func(t *testing.T, scheme, host, port, path string, vSel int) {
		u := &transport.URL{
			Scheme: scheme,
			Host:   host,
			Port:   port,
			Path:   path,
		}
		v := selectProtocolVersion(vSel)

		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		err := WriteStreamRequest(w, u, v)
		if err != nil {
			return
		}

		// A successful encode must emit exactly one data packet whose
		// framing the reader can consume without surfacing a framing
		// error. The payload contents themselves are not asserted —
		// the unit suite covers byte-exact shapes.
		r := pktline.NewReader(bytes.NewReader(buf.Bytes()))
		pkt, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket on successfully-written stream: %v", err)
		}
		if pkt.Kind != pktline.Data {
			t.Fatalf("ReadPacket kind = %v, want Data", pkt.Kind)
		}
		if _, err := r.ReadPacket(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("trailing ReadPacket: want EOF, got %v", err)
		}
	})
}

// FuzzHTTPProtocolHeader exercises [HTTPProtocolHeader] with the four
// [transport.ProtocolVersion] selectors plus an out-of-range value.
// The contract under test is that the function is total — every input
// returns a non-empty string — irrespective of whether the integer
// value is one of the three documented constants.
func FuzzHTTPProtocolHeader(f *testing.F) {
	for vSel := range 5 {
		f.Add(vSel)
	}

	f.Fuzz(func(t *testing.T, vSel int) {
		got := HTTPProtocolHeader(selectProtocolVersion(vSel))
		if got == "" {
			t.Fatalf("HTTPProtocolHeader returned empty string for vSel=%d", vSel)
		}
	})
}

// streamRequestFuzzSeed bundles the five fuzz arguments for
// [FuzzWriteStreamRequest] into one struct so the seed table reads
// like the cases it represents.
type streamRequestFuzzSeed struct {
	scheme, host, port, path string
	vSel                     int
}

// writeStreamRequestFuzzSeeds returns the seed corpus for
// [FuzzWriteStreamRequest]. Each seed pairs a [transport.URL] shape
// with a [transport.ProtocolVersion] selector. The host shapes target
// the IPv6-bracketing branch in `hostParameter`; the version selectors
// span nil (auto-negotiate to v2) and each concrete constant so the
// `version > 0` guard at `connect.c:1294` is exercised in every
// direction.
func writeStreamRequestFuzzSeeds() []streamRequestFuzzSeed {
	return []streamRequestFuzzSeed{
		// Empty host with nil version — the smallest legal input.
		{scheme: "git", host: "", port: "", path: "/repo.git", vSel: 0},
		// IPv4 host without port, v0 (no version trailer).
		{scheme: "git", host: "192.0.2.1", port: "", path: "/repo.git", vSel: 1},
		// IPv4 host with port, v1.
		{scheme: "git", host: "192.0.2.1", port: "9418", path: "/repo.git", vSel: 2},
		// IPv6 literal without port, v2 — bare-literal branch.
		{scheme: "git", host: "::1", port: "", path: "/repo.git", vSel: 3},
		// IPv6 literal with port, v2 — rebracketing branch.
		{scheme: "git", host: "2001:db8::1", port: "9418", path: "/repo.git", vSel: 3},
		// Path without leading slash — `connect.c::git_connect_git`
		// emits `u.Path` verbatim, so the encoder must tolerate it.
		{scheme: "git", host: "example.com", port: "", path: "repo.git", vSel: 3},
		// Path with leading slash and an out-of-range version selector
		// — exercises the `version > 0` branch with an unusual integer.
		{scheme: "git", host: "example.com", port: "", path: "/repo.git", vSel: 4},
	}
}

// selectProtocolVersion maps a fuzz-driven integer onto the four
// [transport.ProtocolVersion] selectors plus an out-of-range fifth
// case. The mapping uses the selector's non-negative remainder modulo
// five so negative integers from the fuzz engine still resolve to a
// defined branch.
func selectProtocolVersion(sel int) *transport.ProtocolVersion {
	switch ((sel % 5) + 5) % 5 {
	case 0:
		return nil
	case 1:
		v := transport.ProtocolV0
		return &v
	case 2:
		v := transport.ProtocolV1
		return &v
	case 3:
		v := transport.ProtocolV2
		return &v
	default:
		v := transport.ProtocolVersion(99)
		return &v
	}
}
