package objfmt

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
)

// VerifyChecksum walks the entire pack and reports an error if its
// trailing hash does not match a fresh hash of every preceding byte.
//
// The trailer is `len(*new(H))` bytes wide — 20 for [SHA1Hash], 32 for
// [SHA256Hash] — per the pack file layout in
// `Documentation/gitformat-pack.adoc`. Verifying the trailer is
// expensive (whole-file read) but it is the only way to prove the
// pack body is intact end-to-end; callers should run it once at open
// time for untrusted packs and lean on the OS page cache for any
// subsequent random-access reads.
func (p *Pack[H]) VerifyChecksum() error {
	var zero H
	hashLen := int64(len(zero))
	if hashLen == 0 {
		return fmt.Errorf("objfmt: unsupported algo %v: %w", p.algo, ErrUnsupportedAlgo)
	}
	totalLen := p.r.Len()
	if totalLen < hashLen+12 {
		return fmt.Errorf("objfmt: pack file too short for trailer: %w", ErrShortFile)
	}

	var h hash.Hash
	switch p.algo {
	case SHA1:
		h = sha1.New()
	case SHA256:
		h = sha256.New()
	default:
		return fmt.Errorf("objfmt: unsupported algo %v: %w", p.algo, ErrUnsupportedAlgo)
	}

	bodyEnd := totalLen - hashLen
	const chunk = 1 << 20
	buf := make([]byte, chunk)
	for off := int64(0); off < bodyEnd; {
		end := min(off+chunk, bodyEnd)
		n, err := p.r.ReadAt(buf[:end-off], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("objfmt: read at %d: %w", off, err)
		}
		h.Write(buf[:n])
		off += int64(n)
		if n == 0 {
			return fmt.Errorf("objfmt: zero-byte read at %d", off)
		}
	}

	want := make([]byte, hashLen)
	if _, err := p.r.ReadAt(want, bodyEnd); err != nil {
		return fmt.Errorf("objfmt: read trailer: %w", err)
	}
	if !bytes.Equal(h.Sum(nil), want) {
		return fmt.Errorf("objfmt: pack trailer mismatch: %w", ErrChecksumMismatch)
	}
	return nil
}
