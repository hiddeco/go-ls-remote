package server

import (
	"fmt"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// writeV2Advertisement emits the discovery-time v2 capability
// advertisement: a `version 2\n` data packet, one data packet per
// advertised capability (each terminated by a single `\n`), and a
// trailing flush. The shape matches `gitprotocol-v2.adoc`
// §"Capability Advertisement" and the framing matches
// `serve.c::protocol_v2_advertise_capabilities` lines 186-216.
//
// The capability set is a strict subset of canonical Git's, in
// canonical Git's emission order from `serve.c:140-185`'s
// `capabilities[]` array — `agent`, `ls-refs`, `object-format`,
// `object-info`. The unsupported capabilities (`fetch`,
// `server-option`, `session-id`, `bundle-uri`, `promisor-remote`)
// are skipped because the emulator is read-only and
// metadata-only. Order matters: a downstream goal is byte-for-byte
// equivalence with `git upload-pack`'s advertisement against the
// same fixture, so the order tracks `serve.c` rather than any
// alternative ordering a reader might find in surrounding design
// notes.
//
//   - `agent=<value>` — opts.Agent when non-empty, otherwise
//     [wire.DefaultUserAgent]. Canonical reference:
//     `serve.c::agent_advertise` lines 25-31.
//   - `ls-refs=unborn` — the emulator implements the `unborn`
//     feature, so it is always advertised. Canonical reference:
//     `ls-refs.c::ls_refs_advertise` lines 218-223.
//   - `object-format=<algo>` — the repository's hash algorithm
//     name (`sha1` or `sha256`). Canonical reference:
//     `serve.c::object_format_advertise` lines 53-58.
//   - `object-info` — boolean, no `=value`. Canonical reference:
//     `serve.c::object_info_advertise` lines 92-101.
func writeV2Advertisement(w *pktline.Writer, store *objstore.Store, opts Options) error {
	if err := w.WritePacket([]byte("version 2\n")); err != nil {
		return fmt.Errorf("server: write v2 version line: %w", err)
	}

	agent := opts.Agent
	if agent == "" {
		agent = wire.DefaultUserAgent
	}
	caps := []string{
		"agent=" + agent,
		"ls-refs=unborn",
		"object-format=" + store.Algo().String(),
		"object-info",
	}
	for _, cap := range caps {
		if err := w.WritePacket([]byte(cap + "\n")); err != nil {
			return fmt.Errorf("server: write v2 capability %q: %w", cap, err)
		}
	}

	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: write v2 advertisement flush: %w", err)
	}
	return nil
}
