package objfmt

import (
	"errors"
	"io"
	"os"

	"golang.org/x/exp/mmap"
)

// packReader is the minimal interface objfmt needs from the on-disk
// store backing a pack or idx file: random-access read plus close.
//
// mmap is preferred — the OS page cache handles the working set across
// concurrent readers without per-call syscall overhead — and a plain
// `*os.File` via [io.ReaderAt] is the fallback used when mmap fails
// (read-only filesystems, 32-bit hosts holding multi-gigabyte packs,
// or platforms where `golang.org/x/exp/mmap` returns an error).
type packReader interface {
	io.ReaderAt
	io.Closer

	// Len returns the size of the underlying file in bytes.
	Len() int
}

// openPackReader opens path for random-access read. It first attempts
// to mmap the file; on any error it falls back to a buffered
// `*os.File`. The returned packReader is safe for concurrent
// [io.ReaderAt.ReadAt] calls.
func openPackReader(path string) (packReader, error) {
	if r, err := mmap.Open(path); err == nil {
		return r, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &fileReader{f: f, size: st.Size()}, nil
}

// fileReader is the non-mmap fallback. ReadAt rejects negative or
// past-end offsets up front so the caller sees a deterministic error
// rather than the platform-specific behaviour of `(*os.File).ReadAt`.
type fileReader struct {
	f    *os.File
	size int64
}

func (r *fileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.size {
		return 0, errors.New("objfmt: read past end of file")
	}
	return r.f.ReadAt(p, off)
}

func (r *fileReader) Len() int     { return int(r.size) }
func (r *fileReader) Close() error { return r.f.Close() }
