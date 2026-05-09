package objstore

// Refname format rules. The validator below mirrors canonical Git's
// `check_refname_format` (`refs.c:320`) called with
// `REFNAME_ALLOW_ONELEVEL`, the same flag the packed-refs iterator uses
// (`refs/packed-backend.c:938`). Refnames are slash-separated path
// components; each component is validated by the equivalent of
// `check_refname_component` (`refs.c:192`), and a few rules apply to the
// refname as a whole.
//
// Per-component rules:
//
//   - Non-empty.
//   - Does not start with `.`.
//   - Does not end with `.lock` (case-sensitive).
//   - Does not contain `..` (consecutive dots).
//   - Does not contain ASCII control characters (0x00–0x1F or 0x7F).
//   - Does not contain space, tab, `~`, `^`, `:`, `?`, `[`, `\`, `*`.
//   - Does not contain `@{`.
//
// Across the whole refname:
//
//   - Not empty.
//   - Does not equal `@`.
//   - Does not end with `.`.
//
// Bytes >= 0x80 are accepted as ordinary component characters (the
// canonical disposition table at `refs.c:80` leaves rows 8–15 zeroed,
// i.e. disposition 0). Empty components — meaning a leading `/`,
// trailing `/`, or `//` — are rejected because the per-component check
// requires non-empty input.
//
// Single-component names like `HEAD` are accepted because the iterator
// runs the validator with `REFNAME_ALLOW_ONELEVEL`. Without that flag
// canonical Git would also reject a refname with fewer than two
// components (`refs.c:315`).
func checkRefnameFormat(name string) bool {
	if name == "" || name == "@" {
		return false
	}
	if name[len(name)-1] == '.' {
		return false
	}
	// Walk the name component by component. The slash is not part of
	// any component; the validator runs once per component and returns
	// false on the first violation.
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '/' {
			continue
		}
		if !checkRefnameComponent(name[start:i]) {
			return false
		}
		start = i + 1
	}
	return true
}

// checkRefnameComponent validates a single slash-separated component of
// a refname. The disposition rules follow `refs.c:192` and the
// `refname_disposition` table on `refs.c:80`. The component is rejected
// when empty, when it begins with `.`, when it ends with `.lock`, when
// it contains `..` or `@{`, or when it contains any forbidden byte.
func checkRefnameComponent(c string) bool {
	if c == "" {
		return false
	}
	if c[0] == '.' {
		return false
	}
	if hasDotLockSuffix(c) {
		return false
	}
	var last byte
	for i := 0; i < len(c); i++ {
		ch := c[i]
		switch {
		case ch < 0x20, ch == 0x7f:
			// ASCII control characters (including NUL).
			return false
		case ch == ' ', ch == '\t':
			return false
		case ch == '~', ch == '^', ch == ':', ch == '?', ch == '[', ch == '\\', ch == '*':
			return false
		case ch == '/':
			// Caller splits on `/`; a slash inside a component would be
			// a programming error.
			return false
		case ch == '.':
			if last == '.' {
				return false
			}
		case ch == '{':
			if last == '@' {
				return false
			}
		}
		last = ch
	}
	return true
}

// hasDotLockSuffix reports whether the component ends with the
// case-sensitive byte sequence `.lock`. Canonical Git uses the
// equivalent `LOCK_SUFFIX` check at `refs.c:264`.
func hasDotLockSuffix(c string) bool {
	const suffix = ".lock"
	if len(c) < len(suffix) {
		return false
	}
	return c[len(c)-len(suffix):] == suffix
}
