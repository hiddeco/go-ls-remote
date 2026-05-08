package objfmt

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

// idxV2OverflowMSB marks an offset entry whose lower 31 bits index
// the 64-bit overflow table rather than encoding the offset directly.
// Per `Documentation/gitformat-pack.adoc` lines 304-312 and the read
// path in `packfile.c::nth_packed_object_offset`.
const idxV2OverflowMSB uint32 = 0x80000000

// findOffsetV2 binary-searches the v2 sorted-name table, falling
// through to the 64-bit overflow table when the small-offset slot
// has bit 31 set.
//
// The math mirrors `nth_packed_object_offset` in canonical Git's
// `packfile.c`: index past the 8-byte header and the 256-entry
// fan-out, then `8 + N*hashLen + N*4 + i*4` lands on the four-byte
// offset slot for record `i`.
func (i *Idx) findOffsetV2(h Hash) (int64, bool) {
	hashLen := i.algo.Size()
	if i.count == 0 || hashLen == 0 {
		return -1, false
	}

	idx, ok := i.searchV2(h)
	if !ok {
		return -1, false
	}

	offsetTable := idxV2HeaderLen + 256*4 + int(i.count)*hashLen + int(i.count)*4
	if len(i.data) < offsetTable+int(i.count)*4 {
		return -1, false
	}
	off := binary.BigEndian.Uint32(i.data[offsetTable+int(idx)*4 : offsetTable+(int(idx)+1)*4])
	if off&idxV2OverflowMSB == 0 {
		return int64(off), true
	}
	overflowIdx := off &^ idxV2OverflowMSB
	overflowTable := offsetTable + int(i.count)*4
	want := overflowTable + int(overflowIdx)*8 + 8
	if len(i.data) < want {
		return -1, false
	}
	big := binary.BigEndian.Uint64(i.data[overflowTable+int(overflowIdx)*8 : want])
	return int64(big), true
}

// FindCRC32 returns the CRC32 of the packed (compressed) bytes of the
// named object as recorded in the idx. ok is false on a miss, on a
// v1 idx (which does not store CRCs), or before the idx is opened.
//
// The CRC table is new in v2 — see `gitformat-pack.adoc` lines
// 299-302 — and lets repacking copy compressed entries verbatim while
// still detecting bit-rot at copy time.
func (i *Idx) FindCRC32(h Hash) (uint32, bool) {
	if i.ver != 2 || i.count == 0 {
		return 0, false
	}
	hashLen := i.algo.Size()
	idx, ok := i.searchV2(h)
	if !ok {
		return 0, false
	}
	crcTable := idxV2HeaderLen + 256*4 + int(i.count)*hashLen
	if len(i.data) < crcTable+int(i.count)*4 {
		return 0, false
	}
	crc := binary.BigEndian.Uint32(i.data[crcTable+int(idx)*4 : crcTable+(int(idx)+1)*4])
	return crc, true
}

// PackChecksum returns the trailer hash of the paired pack file as
// recorded in the idx.
//
// In both v1 and v2 the pack-trailer copy sits immediately before the
// idx self-checksum at the end of the file (`gitformat-pack.adoc`
// lines 213-218 and 314-319). For SHA-1 idxs the result occupies the
// low 20 bytes of the returned [Hash]; the high 12 are zero.
func (i *Idx) PackChecksum() Hash {
	hashLen := i.algo.Size()
	if hashLen == 0 || len(i.data) < 2*hashLen {
		return Hash{}
	}
	var h Hash
	copy(h[:hashLen], i.data[len(i.data)-2*hashLen:len(i.data)-hashLen])
	return h
}

// VerifyChecksum hashes every byte of the idx except the trailing
// `hashLen`-byte self-checksum and compares the result to that
// trailer.
//
// The trailer is the *idx* self-hash, not the pack-trailer copy that
// sits just before it (`gitformat-pack.adoc` lines 314-319). Use
// [Idx.PackChecksum] to read the pack-trailer copy.
func (i *Idx) VerifyChecksum() error {
	hashLen := i.algo.Size()
	if hashLen == 0 {
		return fmt.Errorf("objfmt: unsupported algo %v", i.algo)
	}
	if len(i.data) < hashLen {
		return errors.New("objfmt: idx too short for trailer")
	}

	// v1 only ever stored SHA-1 ids; the trailer is SHA-1 even when
	// the [Idx] was opened with SHA256 (a misuse that [OpenIdx]
	// tolerates for forward-compatibility).
	var h hash.Hash
	switch {
	case i.ver == 1:
		h = sha1.New()
		hashLen = 20
	case i.algo == SHA1:
		h = sha1.New()
	case i.algo == SHA256:
		h = sha256.New()
	default:
		return fmt.Errorf("objfmt: unsupported algo %v", i.algo)
	}

	body := i.data[:len(i.data)-hashLen]
	trailer := i.data[len(i.data)-hashLen:]
	h.Write(body)
	if !bytes.Equal(h.Sum(nil), trailer) {
		return errors.New("objfmt: idx trailer mismatch")
	}
	return nil
}

// searchV2 binary-searches the sorted-name table for h, narrowed by
// the 256-entry fan-out, and returns the matching record index.
func (i *Idx) searchV2(h Hash) (uint32, bool) {
	hashLen := i.algo.Size()
	if hashLen == 0 || i.count == 0 {
		return 0, false
	}
	if len(i.data) < idxV2HeaderLen+256*4 {
		return 0, false
	}

	// Fan-out semantics match v1: fanout[N] = number of entries whose
	// first byte is ≤ N. Hence entries with first byte == prefix
	// occupy [fanout[prefix-1], fanout[prefix]).
	fanout := i.data[idxV2HeaderLen : idxV2HeaderLen+256*4]
	prefix := h[0]
	var lo, hi uint32
	if prefix > 0 {
		lo = binary.BigEndian.Uint32(fanout[(int(prefix)-1)*4 : int(prefix)*4])
	}
	hi = binary.BigEndian.Uint32(fanout[int(prefix)*4 : (int(prefix)+1)*4])
	if hi > i.count {
		return 0, false
	}

	nameTable := idxV2HeaderLen + 256*4
	if len(i.data) < nameTable+int(i.count)*hashLen {
		return 0, false
	}
	want := h[:hashLen]
	for lo < hi {
		mid := (lo + hi) / 2
		got := i.data[nameTable+int(mid)*hashLen : nameTable+(int(mid)+1)*hashLen]
		switch bytes.Compare(want, got) {
		case 0:
			return mid, true
		case -1:
			hi = mid
		case 1:
			lo = mid + 1
		}
	}
	return 0, false
}
