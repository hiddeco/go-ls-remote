package objstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// openAlternates resolves the chain of alternate object stores reachable
// from `<commonDir>/objects/info/alternates` and returns each as a
// fully-constructed [Store]. Every alternate is opened recursively via
// [openWithSeen]; the alternate's own `extensions.objectFormat` is
// re-read by that call, so per-alternate algo inheritance follows
// canonical Git's "each repository decides its own algorithm" rule
// rather than being imposed by the parent.
//
// The seen set carries the canonicalized commonDir of every store
// already visited along the current chain, including the parent that
// triggered this call (seeded by [openWithSeen] before invocation). An
// alternate's commonDir is the parent of its `objects/` directory; a
// repeat match against `seen` is reported as a cycle wrapping
// [ErrCorruptObject]. The canonical Git equivalent
// (`odb.c::odb_is_source_usable`) silently drops the duplicate, but for
// a read-only library callers are better served by surfacing the
// misconfiguration than by quietly papering over it.
//
// Parsing follows `odb.c::parse_alternates`:
//
//   - lines separated by `\n`, with a tolerated trailing `\r`,
//   - a leading `#` makes the line a comment and skips it,
//   - empty lines (after whitespace strip) are skipped,
//   - a leading `"` triggers C-style unquoting; on broken quoting
//     ([unquoteCStyle] returns false) the literal line is used,
//   - relative paths are joined under `<commonDir>/objects/` and
//     realpath'd; absolute paths are realpath'd directly.
//
// A missing alternates file is not an error: alternates are optional
// and most repositories carry none. Other I/O failures wrap the
// underlying error with the offending path. A per-entry open failure
// (the path no longer points at a usable repository, or the cycle
// check trips) closes every alternate already opened on this call so
// the caller never sees a partially-constructed chain.
func openAlternates[H objfmt.Hash](commonDir string, seen map[string]bool) ([]*Store[H], error) {
	path := filepath.Join(commonDir, "objects", "info", "alternates")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("objstore: read %s: %w", path, err)
	}

	// Relative entries resolve against the parent's `objects/` directory
	// (canonical: `odb_source_files_read_alternates` passes
	// `source->path`, which is the objects dir).
	relativeBase := filepath.Join(commonDir, "objects")

	entries := parseAlternates(raw, relativeBase)
	if len(entries) == 0 {
		return nil, nil
	}

	var (
		opened   []*Store[H]
		closeAll = func() {
			for _, s := range opened {
				_ = s.Close()
			}
		}
	)
	for _, objectsDir := range entries {
		// The alternate path names the `objects/` directory of the
		// alternate repository; its enclosing gitdir is the parent of
		// that directory. `Open`/`openWithSeen` accept either a working
		// tree or a gitdir, so passing the parent directly is the
		// canonical handoff.
		altGitDir := filepath.Dir(objectsDir)
		canonical := canonicalRepoDir(altGitDir)
		if seen[canonical] {
			// `seen` carries every store currently being opened along the
			// active alternates chain (each [openWithSeen] frame marks
			// itself in [seen] and pops on return). A hit here is therefore
			// a true back-edge into an in-flight ancestor — a cycle. A
			// duplicate that is reachable through two independent chains
			// (a diamond DAG) is NOT in [seen] at the time the second
			// chain considers it, because the first chain's frame popped
			// before the second chain's [openAlternates] runs.
			closeAll()
			return nil, fmt.Errorf(
				"objstore: alternates cycle through %s: %w",
				canonical, ErrCorruptObject)
		}

		// No options forwarded: per-alternate algo (and every other
		// extension) is read from the alternate's own `config`. The
		// recursive [openWithSeen] marks `canonical` in [seen] and pops
		// on return, so this loop never has to manage the cycle barrier
		// itself.
		alt, err := openWithSeen[H](altGitDir, nil, seen)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("objstore: alternate %s: %w", altGitDir, err)
		}
		opened = append(opened, alt)
	}
	return opened, nil
}

// parseAlternates splits an `objects/info/alternates` payload into the
// fully-resolved object-directory paths it names, mirroring canonical
// Git's `odb.c::parse_alternates`. The grammar is documented on
// [openAlternates]; this helper handles only the byte-level decoding
// (line splitting, comment skipping, C-style unquoting, relative-path
// resolution, realpath canonicalization), leaving repository validation
// and cycle detection to the caller.
func parseAlternates(raw []byte, relativeBase string) []string {
	var out []string
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		// Tolerate CRLF: strip a trailing `\r` so quoted entries on
		// Windows-authored alternates files are still recognised.
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			continue
		}
		if line[0] == '#' {
			continue
		}

		var entry string
		if line[0] == '"' {
			if unquoted, ok := unquoteCStyle(line); ok {
				entry = unquoted
			} else {
				// Broken quoting: canonical Git falls through to the
				// literal line. Match that "be tolerant" behaviour
				// rather than dropping the entry.
				entry = string(line)
			}
		} else {
			entry = string(line)
		}

		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		if !filepath.IsAbs(entry) {
			entry = filepath.Join(relativeBase, entry)
		}

		// Canonicalize: prefer EvalSymlinks so multiple alternate
		// entries resolving to the same on-disk store collide in the
		// `seen` map. Broken or unreadable symlinks fall back to a
		// cleaned absolute path so the entry survives — the recursive
		// `Open` will surface the real failure with a focused error.
		if real, err := filepath.EvalSymlinks(entry); err == nil {
			entry = real
		} else if abs, err := filepath.Abs(filepath.Clean(entry)); err == nil {
			entry = abs
		} else {
			entry = filepath.Clean(entry)
		}

		out = append(out, entry)
	}
	return out
}

// canonicalRepoDir reduces a repository directory path (a commonDir or
// the parent of an alternate's `objects/`) to the canonical form used
// as keys in the alternates `seen` set. EvalSymlinks is preferred so
// two entries linking via different symlinks collapse to one; on
// failure (broken symlinks, unreadable parents) it falls back to an
// absolute cleaned path, then to a plain clean. The fallbacks
// intentionally preserve the path so the caller still gets a
// deterministic key rather than an empty string that would silently
// disable the check.
func canonicalRepoDir(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	if abs, err := filepath.Abs(filepath.Clean(p)); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// unquoteCStyle decodes a C-style quoted byte slice that begins with
// `"`. Returns the unquoted string and true on success, or `("", false)`
// on any malformed input. The accepted escapes mirror canonical Git's
// `quote.c::unquote_c_style`: `\"`, `\\`, `\a`, `\b`, `\f`, `\n`, `\r`,
// `\t`, `\v`, plus three-digit octal `\nnn` whose first digit is in
// `0..3` (so the value fits in a byte).
//
// The function intentionally does not consume trailing bytes after the
// closing quote: the alternates parser feeds whole lines, so anything
// after the close-quote is discarded. Callers needing endp-style
// look-ahead should compose their own driver.
func unquoteCStyle(in []byte) (string, bool) {
	if len(in) == 0 || in[0] != '"' {
		return "", false
	}
	var b strings.Builder
	b.Grow(len(in))
	for i := 1; i < len(in); {
		c := in[i]
		switch c {
		case '"':
			// Closing quote ends the literal. Trailing bytes after
			// the quote (if any) are intentionally ignored.
			return b.String(), true
		case '\\':
			i++
			if i >= len(in) {
				return "", false
			}
			esc := in[i]
			switch esc {
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			case '\\', '"':
				b.WriteByte(esc)
			case '0', '1', '2', '3':
				// Three-digit octal whose first digit is in 0..3 so
				// the value cannot overflow a byte.
				if i+2 >= len(in) {
					return "", false
				}
				d2, d3 := in[i+1], in[i+2]
				if d2 < '0' || d2 > '7' || d3 < '0' || d3 > '7' {
					return "", false
				}
				v := (int(esc-'0') << 6) |
					(int(d2-'0') << 3) |
					int(d3-'0')
				b.WriteByte(byte(v))
				i += 2
			default:
				return "", false
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	// Reached end of input without seeing a closing quote.
	return "", false
}
