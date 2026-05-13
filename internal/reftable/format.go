// Package reftable reads Git reftable files: a stack-of-tables ref
// storage format with binary search inside each table and a merged
// view across the stack.
//
// The package is built up in layers, each on top of the previous:
//
//   - [parseHeader] / [verifyTrailer] decode the fixed-size header
//     that begins every file and validate the CRC-32 trailer that
//     closes the footer.
//   - [parseBlock] decodes a ref/index/obj block header and its
//     restart table; [decodeKey] and [decodeRefRecord] decode the
//     prefix-compressed records inside.
//   - [seekToLeaf] descends the ref index (when present) to the leaf
//     ref block that should contain a sought name, falling back to a
//     linear ref-block walk for files without an index.
//
// The public surface is two read-only types:
//
//   - [Reader] over a single reftable file ([OpenReader]).
//   - [Stack] over a `tables.list` manifest, pre-merging the ref view
//     across every table at construction ([OpenStack]).
//
// Format reference: canonical Git's [Documentation/technical/reftable.adoc]
// and [reftable/table.c::parse_footer].
//
// [Documentation/technical/reftable.adoc]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc
// [reftable/table.c::parse_footer]: https://github.com/git/git/blob/v2.54.0/reftable/table.c#L43
package reftable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Sentinel errors returned by the header and trailer parsers. Callers
// match against these with [errors.Is]; the wrapping
// `fmt.Errorf("...: %w", ..., sentinel)` adds the offending value
// (offsets, declared sizes, magic bytes, version numbers) for
// diagnostics.
var (
	// ErrShortFile is returned when the input is smaller than the
	// fixed header (or, for the trailer, smaller than header + footer).
	// It signals truncation discovered before any field is parsed.
	ErrShortFile = errors.New("reftable: file too short")

	// ErrBadMagic is returned when the four-byte magic at the start of
	// the file is not the ASCII string `REFT`.
	ErrBadMagic = errors.New("reftable: bad magic")

	// ErrUnsupportedVersion is returned when the version byte is
	// neither 1 nor 2.
	ErrUnsupportedVersion = errors.New("reftable: unsupported version")

	// ErrBadHashID is returned when a v2 file's `hash_id` field is
	// neither the ASCII tag `sha1` nor `s256`.
	ErrBadHashID = errors.New("reftable: bad hash id")

	// ErrTrailerChecksum is returned when the CRC-32 stored at the end
	// of the footer does not match a fresh CRC computed over the
	// preceding footer bytes.
	ErrTrailerChecksum = errors.New("reftable: trailer checksum mismatch")
)

// [reftable.adoc § Header (version 1)] / [reftable.adoc § Header (version 2)] — the
// header is 24 bytes for v1 and 28 bytes for v2 (an extra uint32
// `hash_id`). Footer length is 5 × uint64 of section offsets plus a
// uint32 CRC, prefixed with a copy of the header: 68 / 72 bytes.
//
// [reftable.adoc § Header (version 1)]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#header-version-1
// [reftable.adoc § Header (version 2)]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#header-version-2
const (
	magic = "REFT"

	headerSizeV1 = 24
	headerSizeV2 = 28

	// Footer = header copy + 5 * 8 + 4-byte CRC.
	footerSizeV1 = headerSizeV1 + 5*8 + 4 // 68
	footerSizeV2 = headerSizeV2 + 5*8 + 4 // 72

	// hash_id constants: 4-byte ASCII tags carried as a big-endian
	// uint32 in v2 headers. [reftable.adoc § Header (version 2)].
	//
	// [reftable.adoc § Header (version 2)]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#header-version-2
	hashIDSHA1   uint32 = 0x73686131 // 'sha1'
	hashIDSHA256 uint32 = 0x73323536 // 's256'
)

// header is the parsed form of the fixed-size prefix that begins
// every reftable file. It is private to the package; higher layers
// thread a header value through the lower-level helpers without
// exposing the on-disk shape to callers.
type header struct {
	version        uint8
	blockSize      uint32 // uint24 on disk
	minUpdateIndex uint64
	maxUpdateIndex uint64
	algo           objfmt.Algo
}

// size returns the on-disk size of h's header in bytes — 24 for v1
// and 28 for v2. The footer begins with a byte-identical copy of this
// many bytes (see [verifyTrailer]).
func (h header) size() int {
	if h.version == 2 {
		return headerSizeV2
	}
	return headerSizeV1
}

// footerSize returns the on-disk size of h's footer in bytes — 68 for
// v1 and 72 for v2.
func (h header) footerSize() int {
	if h.version == 2 {
		return footerSizeV2
	}
	return footerSizeV1
}

// parseHeader decodes the fixed-size header that begins a reftable
// file from the start of buf. It accepts either the full file or just
// the leading bytes; it reads at most the v2 header length (28 bytes).
//
// The returned [header] carries the version, block size, update-index
// bounds, and resolved hash [objfmt.Algo]. v1 files have no on-disk
// hash_id; the algo is implicitly SHA-1 per [reftable.adoc § Header (version 1)].
// v2 files carry the four-byte ASCII tag `sha1` or
// `s256` after `max_update_index`; any other tag is [ErrBadHashID].
//
// [reftable.adoc § Header (version 1)]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#header-version-1
func parseHeader(buf []byte) (header, error) {
	if len(buf) < headerSizeV1 {
		return header{}, fmt.Errorf("reftable: have %d bytes, need %d for header: %w", len(buf), headerSizeV1, ErrShortFile)
	}
	if string(buf[:4]) != magic {
		return header{}, fmt.Errorf("reftable: magic %q != %q: %w", buf[:4], magic, ErrBadMagic)
	}

	ver := buf[4]
	switch ver {
	case 1, 2:
	default:
		return header{}, fmt.Errorf("reftable: version %d: %w", ver, ErrUnsupportedVersion)
	}

	if ver == 2 && len(buf) < headerSizeV2 {
		return header{}, fmt.Errorf("reftable: have %d bytes, need %d for v2 header: %w", len(buf), headerSizeV2, ErrShortFile)
	}

	h := header{
		version:        ver,
		blockSize:      uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7]),
		minUpdateIndex: binary.BigEndian.Uint64(buf[8:16]),
		maxUpdateIndex: binary.BigEndian.Uint64(buf[16:24]),
	}

	if ver == 1 {
		h.algo = objfmt.SHA1
		return h, nil
	}

	switch tag := binary.BigEndian.Uint32(buf[24:28]); tag {
	case hashIDSHA1:
		h.algo = objfmt.SHA1
	case hashIDSHA256:
		h.algo = objfmt.SHA256
	default:
		return header{}, fmt.Errorf("reftable: hash id %#08x: %w", tag, ErrBadHashID)
	}
	return h, nil
}

// verifyTrailer checks the integrity of the footer that closes a
// reftable file. file must hold the entire reftable bytes; h must be
// the header already parsed from file's prefix.
//
// The footer begins with a byte-identical copy of the header
// ([reftable.adoc § Footer]); verifyTrailer rejects files where the
// two diverge. The trailing four bytes are a CRC-32 over every
// preceding footer byte — i.e. the header copy plus the five 8-byte
// section offsets. Trailer integrity is CRC-32 (poly 0xedb88320,
// stdlib `hash/crc32` with [crc32.IEEETable]); the spec calls this
// the "trailer hash" but the canonical implementation in
// [reftable/table.c::parse_footer] uses CRC-32.
//
// [reftable.adoc § Footer]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#footer
// [reftable/table.c::parse_footer]: https://github.com/git/git/blob/v2.54.0/reftable/table.c#L43
func verifyTrailer(file []byte, h header) error {
	footerLen := h.footerSize()
	headerLen := h.size()

	if len(file) < headerLen+footerLen {
		return fmt.Errorf("reftable: have %d bytes, need at least %d for header and footer: %w", len(file), headerLen+footerLen, ErrShortFile)
	}

	footer := file[len(file)-footerLen:]

	// [reftable.adoc § Footer] — the footer begins with a copy of the
	// file header. Canonical Git checks this with memcmp before
	// parsing footer fields; mismatch is a format error.
	//
	// [reftable.adoc § Footer]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#footer
	if !bytes.Equal(footer[:headerLen], file[:headerLen]) {
		return fmt.Errorf("reftable: footer header copy diverges from file header")
	}

	// CRC-32 covers every footer byte before the trailing 4-byte CRC
	// itself. [reftable/table.c:106] — `crc32(0, footer, f - footer)`.
	//
	// [reftable/table.c:106]: https://github.com/git/git/blob/v2.54.0/reftable/table.c#L106
	want := binary.BigEndian.Uint32(footer[footerLen-4:])
	got := crc32.Checksum(footer[:footerLen-4], crc32.IEEETable)
	if got != want {
		return fmt.Errorf("reftable: trailer crc %#08x != %#08x: %w", got, want, ErrTrailerChecksum)
	}
	return nil
}
