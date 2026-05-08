package objfmt

import (
	"bufio"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"strconv"
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
	size, err = strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		_ = zr.Close()
		return 0, 0, nil, fmt.Errorf("objfmt: parse size %q: %w", sizeStr, err)
	}
	if size < 0 {
		_ = zr.Close()
		return 0, 0, nil, fmt.Errorf("objfmt: negative loose object size %d", size)
	}

	return typ, size, &looseBody{r: br, closer: zr}, nil
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

func (b *looseBody) Close() error {
	if b.closer == nil {
		return nil
	}
	c := b.closer
	b.closer = nil
	if err := c.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return err
	}
	return nil
}
