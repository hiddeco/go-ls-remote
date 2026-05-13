//go:build live

package livetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// TestProtocolVersion asserts that every provider in [Providers]
// negotiates [lsremote.ProtocolV2] over its unauthenticated HTTPS
// endpoint. The check catches the rare case where a host regresses
// to v0 or v1 — a regression invisible to the per-command tests
// because most commands work transparently across versions.
//
// Iteration stops at one sub-test level (`t.Run(p.Name, ...)`): the
// negotiated protocol version is a provider-level property, not a
// per-credential one, so reusing [forEachProviderMode] would add
// redundant matrix expansion. The dial goes through [lsremote.Dial]
// directly so the test can inspect [lsremote.Session.Capabilities]
// after the handshake, which the one-shot helpers do not expose.
//
// The `t.Logf` line records both the negotiated version and the
// curated v2 command set the server advertised. This is a useful
// diagnostic when reading `-v` output: it surfaces, for example,
// which providers currently advertise `object-info` versus those
// that omit it.
func TestProtocolVersion(t *testing.T) {
	t.Parallel()

	for _, p := range Providers {
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()

			p.skipIfOffline(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			s, err := lsremote.Dial(ctx, p.PublicHTTPS)
			require.NoErrorf(t, err, "%s: Dial failed", p.Name)
			defer func() { _ = s.Close() }()
			caps := s.Capabilities()
			assert.Equalf(t, lsremote.ProtocolV2, caps.Version,
				"%s: expected ProtocolV2, negotiated %v", p.Name, caps.Version)
			t.Logf("%s: negotiated %v (commands=%v)", p.Name, caps.Version, caps.Commands)
		})
	}
}
