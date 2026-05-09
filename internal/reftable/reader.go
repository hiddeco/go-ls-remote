package reftable

import (
	"bytes"
	"fmt"
	"iter"

	"golang.org/x/exp/mmap"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// RefRecord is the public, read-only view of one reference stored in a
// reftable file.
//
// Three shapes are observable: a value record (Value populated, Peeled
// and TargetRef zero); a peeled-tag record (Value and Peeled populated,
// TargetRef zero); and a symbolic-ref record (TargetRef populated,
// Value and Peeled zero). Tombstones — the on-disk markers used to
// shadow a name in a later table — are never returned to callers; the
// reader filters them out so [Reader.IterRefs] and [Reader.FindRef]
// only ever yield observable refs.
//
// The Hash fields follow [objfmt.Hash] conventions: SHA-1 ids occupy
// the low 20 bytes with the high 12 bytes zero, SHA-256 ids fill all
// 32. The [Reader.HashAlgo] return value tells the caller how to
// interpret them.
type RefRecord struct {
	Name      string
	Value     objfmt.Hash
	TargetRef string
	Peeled    objfmt.Hash
}

// Reader is a read-only view of a single reftable file.
//
// A Reader memory-maps its file at construction and keeps the mapping
// live for the entire lifetime of the value; [Reader.Close] releases
// the mapping. Read methods are safe for concurrent use by multiple
// goroutines, but no read may overlap with [Reader.Close].
//
// Reader does not merge across tables — that is the job of a future
// stack reader. A single Reader sees only the records present in the
// file it was opened on, including any tombstones (which are filtered
// out before the public surface).
type Reader struct {
	mmap   *mmap.ReaderAt
	file   []byte
	header header
}

// OpenReader memory-maps the reftable at path, validates its header
// and trailer, and returns a [*Reader] ready for [Reader.IterRefs] and
// [Reader.FindRef] calls.
//
// The header is parsed up-front so HashAlgo and update-index bounds
// are available without further I/O; the trailer is verified via its
// CRC-32 so corrupted files surface as [ErrTrailerChecksum] before any
// record is decoded. Errors from the underlying [mmap.Open] propagate
// unwrapped (e.g. fs.ErrNotExist for a missing path).
//
// On any error after the mmap succeeds, the mapping is closed before
// the error is returned; callers do not need to call [Reader.Close]
// on a failed open.
func OpenReader(path string) (*Reader, error) {
	rdr, err := mmap.Open(path)
	if err != nil {
		return nil, err
	}

	size := rdr.Len()
	if size < headerSizeV1 {
		rdr.Close()
		return nil, fmt.Errorf("reftable: have %d bytes, need %d for header: %w", size, headerSizeV1, ErrShortFile)
	}

	file := make([]byte, size)
	if _, err := rdr.ReadAt(file, 0); err != nil {
		rdr.Close()
		return nil, fmt.Errorf("reftable: read %s: %w", path, err)
	}

	h, err := parseHeader(file)
	if err != nil {
		rdr.Close()
		return nil, err
	}
	if err := verifyTrailer(file, h); err != nil {
		rdr.Close()
		return nil, err
	}

	return &Reader{mmap: rdr, file: file, header: h}, nil
}

// Close releases the memory mapping that backs r.
//
// Close is idempotent: calling it on an already-closed Reader returns
// nil without touching the OS. Calling any read method after Close is
// undefined; callers must serialize Close against in-flight reads.
func (r *Reader) Close() error {
	if r.mmap == nil {
		return nil
	}
	m := r.mmap
	r.mmap = nil
	r.file = nil
	return m.Close()
}

// HashAlgo returns the hash algorithm declared by the file header.
//
// The result is [objfmt.SHA1] for v1 files and either [objfmt.SHA1]
// or [objfmt.SHA256] for v2 files (decoded from the on-disk hash_id
// tag). Callers use it to interpret the [RefRecord.Value] and
// [RefRecord.Peeled] fields.
func (r *Reader) HashAlgo() objfmt.Algo {
	return r.header.algo
}

// IterRefs returns an iterator that yields every observable ref in the
// file in on-disk (sorted) order.
//
// The iterator walks each ref block from the start of the file in
// turn, decoding records sequentially and yielding non-tombstone
// records as [RefRecord] values. Tombstone records (value_type=0) are
// elided: they have no observable meaning in a single file and exist
// only as shadowing markers within a stack.
//
// Errors short-circuit the walk: on a decode failure the iterator
// yields one (RefRecord{}, err) pair and stops. Successful walks emit
// only (rec, nil) pairs and terminate by exhausting the ref blocks.
//
// Breaking out of the range loop is supported and leaks no goroutines:
// the iterator is a pure stack-based [iter.Seq2] that returns control
// when the consumer's yield returns false.
func (r *Reader) IterRefs() iter.Seq2[RefRecord, error] {
	return func(yield func(RefRecord, error) bool) {
		for rec, err := range r.iterAllRefs() {
			if err != nil {
				yield(RefRecord{}, err)
				return
			}
			if rec.ValueType == refValueDeletion {
				continue
			}
			if !yield(liftRefRecord(rec), nil) {
				return
			}
		}
	}
}

// iterAllRefs walks every ref_record in the file in on-disk (sorted)
// order and yields the raw, internal [refRecord] — including tombstones
// (value_type=0). It is the shared engine behind [Reader.IterRefs] and
// [Stack]'s merged-view construction; the public iterator filters
// tombstones, the stack uses them to delete shadowed entries.
//
// The walker scans ref blocks sequentially from the file header
// onwards, stopping at the first non-ref block (obj/index/footer).
// Block advancement uses blockLen, with the first block's length
// folding in the file header preamble; thereafter pos rounds up to
// blockSize when set.
func (r *Reader) iterAllRefs() iter.Seq2[refRecord, error] {
	return func(yield func(refRecord, error) bool) {
		headerLen := uint32(r.header.size())
		hashSize := r.header.algo.Size()

		pos := uint32(0)
		first := true
		for pos < uint32(len(r.file)) {
			firstByteOffset := uint32(0)
			var slice []byte
			if first {
				firstByteOffset = headerLen
				slice = r.file
			} else {
				slice = r.file[pos:]
			}

			if uint32(len(slice)) <= firstByteOffset {
				return
			}
			if slice[firstByteOffset] != blockTypeRef {
				return
			}

			blk, err := parseBlock(slice, firstByteOffset)
			if err != nil {
				yield(refRecord{}, fmt.Errorf("reftable: parse ref block at %d: %w", pos, err))
				return
			}

			recordsEnd := blk.header.blockLen - 2 - 3*uint32(blk.header.restartCount)
			cur := firstByteOffset + 4
			var prevKey []byte
			for cur < recordsEnd {
				rec, key, n, err := decodeRefRecord(blk.bytes[cur:recordsEnd], prevKey, r.header.minUpdateIndex, hashSize)
				if err != nil {
					yield(refRecord{}, fmt.Errorf("reftable: decode ref_record at %d: %w", pos+cur, err))
					return
				}
				if !yield(rec, nil) {
					return
				}
				prevKey = key
				cur += uint32(n)
			}

			// For the first ref block, blockLen folds in the file
			// header preamble (firstByteOffset bytes), so adding it to
			// pos=0 lands precisely at the second block's start. For
			// every later block, blockLen excludes the file header and
			// adds atop pos directly.
			nextPos := pos + blk.header.blockLen
			if r.header.blockSize > 0 {
				nextPos = roundUp(nextPos, r.header.blockSize)
			}
			if nextPos <= pos {
				yield(refRecord{}, fmt.Errorf("reftable: block at %d does not advance: %w", pos, ErrTruncatedBlock))
				return
			}
			pos = nextPos
			first = false
		}
	}
}

// FindRef looks up name in the file and returns the matching record.
//
// FindRef descends the ref index when the file carries one (a
// `ref_index_position` of zero in the footer means "no index", in
// which case ref blocks are walked linearly), then linearly scans the
// resulting leaf block for an exact-name match.
//
// The boolean return distinguishes "no match" (false, nil) from
// "match" (true, nil) and "lookup error" (false, non-nil err). A
// tombstone hit is reported as no-match: tombstones shadow records
// across a stack but, considered alone, leave the name unobservable.
func (r *Reader) FindRef(name string) (RefRecord, bool, error) {
	probe := []byte(name)
	indexRoot := readRefIndexPosition(r.file, r.header)

	leaf, firstByteOffset, err := seekToLeaf(r.file, r.header, indexRoot, probe, nil)
	if err != nil {
		return RefRecord{}, false, err
	}

	blk, err := parseBlock(leaf, firstByteOffset)
	if err != nil {
		return RefRecord{}, false, err
	}
	if blk.header.blockType != blockTypeRef {
		return RefRecord{}, false, fmt.Errorf("reftable: expected ref block, got %q: %w", blk.header.blockType, ErrBadBlockType)
	}

	hashSize := r.header.algo.Size()
	recordsEnd := blk.header.blockLen - 2 - 3*uint32(blk.header.restartCount)
	cur := firstByteOffset + 4
	var prevKey []byte
	for cur < recordsEnd {
		rec, key, n, err := decodeRefRecord(blk.bytes[cur:recordsEnd], prevKey, r.header.minUpdateIndex, hashSize)
		if err != nil {
			return RefRecord{}, false, fmt.Errorf("reftable: decode ref_record at %d: %w", cur, err)
		}
		switch cmp := bytes.Compare(key, probe); {
		case cmp == 0:
			if rec.ValueType == refValueDeletion {
				return RefRecord{}, false, nil
			}
			return liftRefRecord(rec), true, nil
		case cmp > 0:
			// Records are sorted; the probe cannot appear later in the
			// block.
			return RefRecord{}, false, nil
		}
		prevKey = key
		cur += uint32(n)
	}
	return RefRecord{}, false, nil
}

// liftRefRecord converts the decoder's internal refRecord into the
// public [RefRecord]. Per-value-type fields are populated only for the
// types that carry them; tombstones (value_type=0) lift to the zero
// RefRecord and are filtered upstream rather than surfaced as records.
func liftRefRecord(r refRecord) RefRecord {
	out := RefRecord{Name: r.Name}
	switch r.ValueType {
	case refValueSingle:
		out.Value = r.Value
	case refValuePeeled:
		out.Value = r.Value
		out.Peeled = r.Peeled
	case refValueSymref:
		out.TargetRef = r.Target
	}
	return out
}
