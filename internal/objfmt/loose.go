package objfmt

import (
	"bufio"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
)

// ReadLooseHeader reads the type and size header from a zlib-compressed
// loose Git object on r and returns a reader for the raw body.
//
// The decompressed loose object is `<type-name> <size>\0<raw data>`,
// where `<type-name>` is one of `commit`, `tree`, `blob`, or `tag` and
// `<size>` is the ASCII-decimal byte length of the body. See the writer
// side `format_object_header` in canonical Git's `object-file.c:111-120`.
//
// On success body yields the raw object bytes (no header). Closing body
// also closes the underlying zlib reader, so callers that only want the
// type and size may close body without draining it. Close is safe to
// call more than once.
//
// On error body is nil and the wrapped error chain reflects the cause:
// zlib framing, header parsing, or unexpected EOF inside the stream.
func ReadLooseHeader(r io.Reader) (typ ObjectType, size int64, body io.ReadCloser, err error) {
	zr, err := zlib.NewReader(r)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("objfmt: zlib init: %w", err)
	}
	br := bufio.NewReader(zr)

	typeName, err := br.ReadString(' ')
	if err != nil {
		_ = zr.Close()
		return 0, 0, nil, fmt.Errorf("objfmt: read type: %w", err)
	}
	typeName = typeName[:len(typeName)-1] // drop trailing space
	typ, err = parseLooseTypeName(typeName)
	if err != nil {
		_ = zr.Close()
		return 0, 0, nil, err
	}

	sizeStr, err := br.ReadString(0)
	if err != nil {
		_ = zr.Close()
		return 0, 0, nil, fmt.Errorf("objfmt: read size: %w", err)
	}
	sizeStr = sizeStr[:len(sizeStr)-1] // drop trailing NUL
	size, err = parseLooseSize(sizeStr)
	if err != nil {
		_ = zr.Close()
		return 0, 0, nil, err
	}

	return typ, size, &looseBody{r: br, closer: zr}, nil
}

// parseLooseSize decodes the size field of a loose-object header,
// accepting only canonical decimal: a single `0`, or one or more digits
// where the first digit is in `1`..`9`. Leading zeros (`010`), a leading
// sign, surrounding whitespace, and any non-digit character are
// rejected.
//
// Mirrors the manual digit-by-digit loop in canonical Git's
// `object-file.c:369-380` (`parse_loose_header`), which comments "The
// length must follow immediately, and be in canonical decimal format
// (ie '010' is not valid)." `strconv.ParseInt` is too permissive for
// the same input — it tolerates leading zeros, leading `+`, and
// surrounding whitespace — so the validation is rolled here.
func parseLooseSize(s string) (int64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("objfmt: empty loose object size: %w", ErrCorrupt)
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("objfmt: non-canonical loose object size %q: %w", s, ErrCorrupt)
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("objfmt: non-canonical loose object size %q: %w", s, ErrCorrupt)
		}
		// Overflow guard mirrors `st_add(st_mult(size, 10), c)` in
		// `parse_loose_header`. The multiplication is checked before
		// the addition; either overflow rejects the header.
		if n > (math.MaxInt64-int64(c-'0'))/10 {
			return 0, fmt.Errorf("objfmt: loose object size %q overflows int64: %w", s, ErrCorrupt)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// parseLooseTypeName maps the loose-object type-name field to an
// [ObjectType]. Loose objects only carry the four non-delta types;
// `ofs-delta` and `ref-delta` exist solely inside packs.
func parseLooseTypeName(name string) (ObjectType, error) {
	switch name {
	case "commit":
		return TypeCommit, nil
	case "tree":
		return TypeTree, nil
	case "blob":
		return TypeBlob, nil
	case "tag":
		return TypeTag, nil
	default:
		return 0, fmt.Errorf("objfmt: unknown loose object type %q", name)
	}
}

// looseBody adapts the post-header bufio reader and the underlying
// zlib reader into a single [io.ReadCloser] whose Close releases the
// zlib decoder.
type looseBody struct {
	r      io.Reader
	closer io.Closer
}

func (b *looseBody) Read(p []byte) (int, error) { return b.r.Read(p) }

// Close releases the zlib decoder. zlib reports trailer corruption
// (Adler-32 mismatch, dangling continuation bits) at Close time, so
// the error is wrapped with package context rather than swallowed
// or returned bare. [io.ErrClosedPipe] is the one expected
// non-error: zlib emits it when Close is called after the stream is
// already drained or after an earlier Close, which is benign for
// the "type-and-size only" use case documented on
// [ReadLooseHeader].
func (b *looseBody) Close() error {
	if b.closer == nil {
		return nil
	}
	c := b.closer
	b.closer = nil
	if err := c.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return fmt.Errorf("objfmt: close loose object: %w", err)
	}
	return nil
}
