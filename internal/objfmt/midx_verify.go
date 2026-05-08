package objfmt

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"hash"
)

// VerifyChecksum hashes every byte of the midx except the trailing
// `algo.Size()`-byte trailer and compares the result to that trailer.
//
// Mirrors `Pack.VerifyChecksum` and `Idx.VerifyChecksum`: the trailer
// is a fresh hash over the entire body and proves the file has not
// been silently corrupted on disk. Canonical Git computes the same
// trailer in `midx-write.c` (`finalize_hashfile`), so an intact midx
// emitted by `git multi-pack-index write` always verifies.
func (m *Midx) VerifyChecksum() error {
	hashLen := m.algo.Size()
	if hashLen == 0 {
		return fmt.Errorf("objfmt: unsupported algo %v: %w", m.algo, ErrUnsupportedAlgo)
	}
	if len(m.data) < hashLen {
		return fmt.Errorf("objfmt: midx too short for trailer: %w", ErrShortFile)
	}

	var h hash.Hash
	switch m.algo {
	case SHA1:
		h = sha1.New()
	case SHA256:
		h = sha256.New()
	default:
		return fmt.Errorf("objfmt: unsupported algo %v: %w", m.algo, ErrUnsupportedAlgo)
	}

	body := m.data[:len(m.data)-hashLen]
	trailer := m.data[len(m.data)-hashLen:]
	h.Write(body)
	if !bytes.Equal(h.Sum(nil), trailer) {
		return fmt.Errorf("objfmt: midx trailer mismatch: %w", ErrChecksumMismatch)
	}
	return nil
}
