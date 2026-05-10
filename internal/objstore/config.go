package objstore

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// storeConfig is the subset of `config` keys the object store needs to
// open a repository: the object hash algorithm, the ref-storage
// decision, and the option-derived behaviour flags layered on top by
// [Open]. It is populated by [readGitConfig] (algo, refStorage) and
// then mutated by each [Option] before backend construction.
type storeConfig struct {
	// algo is the object hash algorithm selected by
	// `extensions.objectFormat`. Defaults to [objfmt.SHA1] when the
	// key is absent or empty.
	algo objfmt.Algo

	// refStorage describes the ref backend to instantiate, derived
	// from `extensions.refStorage`. Always populated; the zero value
	// of [refStorageSpec] is never returned.
	refStorage refStorageSpec

	// verifyCRC enables per-object CRC32 verification on pack-index
	// reads. Defaults to true; flipped to false by [WithoutCRCCheck].
	// The flag lives on storeConfig (not a sibling type) so backend
	// constructors that already receive the config can consult it
	// without a second parameter.
	verifyCRC bool
}

// refStorageSpec is the parsed `extensions.refStorage` value. It
// distinguishes the bare-name form (`files`, `reftable`) from the
// `<format>://<payload>` URI form, preserving the raw payload string
// for the caller to resolve against the gitdir.
type refStorageSpec struct {
	// format is the lowercase backend name — `"files"` or
	// `"reftable"`. Never empty after a successful parse.
	format string

	// location is the optional explicit storage path extracted from a
	// `<format>://<payload>` URI. Empty when the value was a bare
	// format name. The string is intentionally not resolved here; the
	// store opener interprets relative paths against the gitdir.
	location string
}

// DiscoverAlgo reads `extensions.objectFormat` from the on-disk
// config at path's repository (resolved through [resolveGitDir]) and
// returns the matching [objfmt.Algo]. The transport-layer dispatch
// in `transport/{file,http}` uses this to instantiate the right
// `Store[H]` without opening the rest of the backends first.
//
// Errors propagate from the path resolution and the config parser.
// A missing file or absent `extensions` section is not an error: the
// canonical default of [objfmt.SHA1] is returned.
func DiscoverAlgo(path string) (objfmt.Algo, error) {
	_, commonDir, err := resolveGitDir(path)
	if err != nil {
		return nil, err
	}
	cfg, err := readGitConfig(commonDir)
	if err != nil {
		return nil, err
	}
	return cfg.algo, nil
}

// readGitConfig reads <commonDir>/config and extracts the two keys
// the object store cares about: `extensions.objectFormat` and
// `extensions.refStorage`. It returns the populated [storeConfig] or
// an error wrapping [ErrUnsupportedFormat] when a value is recognised
// as a key but holds an unhandled format token.
//
// A missing config file or a missing `[extensions]` section is treated
// as all-defaults — SHA-1 objects, files-backed refs — to match
// canonical Git's behaviour for repositories created without the
// `core.repositoryFormatVersion = 1` opt-in.
//
// The parser is a deliberately minimal `git-config` reader: it
// recognises only the section-header / `key = value` shape, single-
// line comments introduced by `#` or `;`, and optional double-quotes
// surrounding the value. Multi-line continuations, interpreted-string
// escapes, and multi-value keys are not supported because none of the
// keys consulted here use them. See [parseExtensions] for the
// concrete grammar.
func readGitConfig(commonDir string) (storeConfig, error) {
	cfg := storeConfig{
		algo:       objfmt.SHA1,
		refStorage: refStorageSpec{format: "files"},
		verifyCRC:  true,
	}

	path := filepath.Join(commonDir, "config")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return storeConfig{}, fmt.Errorf("objstore: read %s: %w", path, err)
	}
	defer f.Close()

	raw, err := parseExtensions(f)
	if err != nil {
		return storeConfig{}, fmt.Errorf("objstore: read %s: %w", path, err)
	}

	if v, ok := raw["objectformat"]; ok && v != "" {
		algo, err := parseObjectFormat(v)
		if err != nil {
			return storeConfig{}, fmt.Errorf("objstore: %s: %w", path, err)
		}
		cfg.algo = algo
	}

	if v, ok := raw["refstorage"]; ok && v != "" {
		spec, err := parseRefStorage(v)
		if err != nil {
			return storeConfig{}, fmt.Errorf("objstore: %s: %w", path, err)
		}
		cfg.refStorage = spec
	}

	return cfg, nil
}

// parseObjectFormat maps the raw `extensions.objectFormat` value to an
// [objfmt.Algo]. Comparison is case-insensitive, matching canonical
// Git's `parse_extension_value` in `setup.c`. Anything other than
// `sha1` or `sha256` returns [ErrUnsupportedFormat].
func parseObjectFormat(value string) (objfmt.Algo, error) {
	switch strings.ToLower(value) {
	case "sha1":
		return objfmt.SHA1, nil
	case "sha256":
		return objfmt.SHA256, nil
	default:
		return nil, fmt.Errorf("extensions.objectFormat=%q: %w",
			value, ErrUnsupportedFormat)
	}
}

// parseRefStorage classifies the raw `extensions.refStorage` value.
// It accepts a bare format name (`files`, `reftable`) or the
// `<format>://<payload>` URI form documented in canonical Git's
// `Documentation/config/extensions.adoc` § `extensions.refStorage`.
//
// The payload is returned verbatim. Resolving relative payload paths
// against the gitdir is the store opener's responsibility, so this
// parser stays oblivious to filesystem layout.
func parseRefStorage(value string) (refStorageSpec, error) {
	scheme, payload, hasURI := strings.Cut(value, "://")
	if !hasURI {
		// Bare format name — no payload.
		scheme = value
	}
	switch strings.ToLower(scheme) {
	case "files":
		return refStorageSpec{format: "files", location: payload}, nil
	case "reftable":
		return refStorageSpec{format: "reftable", location: payload}, nil
	default:
		return refStorageSpec{}, fmt.Errorf(
			"extensions.refStorage=%q: %w", value, ErrUnsupportedFormat)
	}
}

// parseExtensions reads a `git-config` stream and returns a map of
// the variables in the `[extensions]` section, lower-cased for
// case-insensitive lookup. Other sections are skipped silently;
// canonical Git would parse them, but this reader only surfaces the
// two keys [readGitConfig] needs.
//
// Grammar accepted:
//
//   - Section headers `[name]` or `[name "subsection"]`. Section names
//     are case-insensitive per `git-config(1)`. Subsections are
//     ignored — no `[extensions]` subsections are defined.
//   - Variable lines `key = value`, with arbitrary surrounding
//     whitespace. Variable names are case-insensitive.
//   - Comments introduced by `#` or `;`, valid anywhere outside a
//     quoted value.
//   - Optional double-quotes surrounding the value, stripped on the
//     way out so `"reftable"` and `reftable` parse identically.
//
// Not supported: backslash escapes inside quoted values, multi-line
// continuations (`\` at end of line), and the boolean shorthand
// `key` (no `=`). The keys this reader cares about — `objectFormat`
// and `refStorage` — never use those forms in practice.
func parseExtensions(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)

	scanner := bufio.NewScanner(r)
	// Allow generously large config lines; canonical Git imposes no
	// hard limit, and a 1 MiB ceiling comfortably covers any realistic
	// `[extensions]` value while still bounding pathological input.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	inExtensions := false
	for scanner.Scan() {
		line := stripComment(scanner.Text())
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") {
			name, ok := parseSectionHeader(line)
			if !ok {
				// Malformed header — skip silently. The keys this
				// reader cares about will simply remain unset, and
				// the caller will fall back to defaults.
				inExtensions = false
				continue
			}
			inExtensions = strings.EqualFold(name, "extensions")
			continue
		}

		if !inExtensions {
			continue
		}

		key, value, ok := splitKeyValue(line)
		if !ok {
			// Boolean shorthand `key` with no `=` is not in scope for
			// the keys this reader handles; ignore the line rather
			// than fail the whole parse.
			continue
		}
		out[strings.ToLower(key)] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// stripComment removes the trailing `#` or `;` comment from line.
// Quoted values are not honoured because the keys this reader cares
// about never embed `#` or `;` inside their values; if that ever
// changes, this function needs to grow a quote-aware scanner.
func stripComment(line string) string {
	if i := strings.IndexAny(line, "#;"); i >= 0 {
		return line[:i]
	}
	return line
}

// parseSectionHeader extracts the section name from a `[name]` or
// `[name "subsection"]` header. The trailing `]` must be present;
// anything else returns ok=false.
func parseSectionHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return "", false
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	if inner == "" {
		return "", false
	}
	// Split off any `"subsection"` suffix; the section name is the
	// leading identifier. We do not validate the subsection contents.
	if i := strings.IndexAny(inner, " \t"); i >= 0 {
		inner = inner[:i]
	}
	return inner, true
}

// splitKeyValue splits a `key = value` line. It returns ok=false when
// no `=` is present (i.e. the boolean shorthand). Surrounding
// whitespace is trimmed from both halves, and a single layer of
// double-quotes around the value is stripped — without honouring
// backslash escapes, which are out of scope.
func splitKeyValue(line string) (key, value string, ok bool) {
	rawKey, rawValue, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(rawKey)
	value = strings.TrimSpace(rawValue)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	return key, value, true
}
