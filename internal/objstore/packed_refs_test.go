package objstore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// TestParsePackedRefs pins down the parser's contract directly, free of
// the loose-refs fixture machinery that exercises it indirectly. The
// table covers header-line traits, body line-ending tolerance, peel
// handling, separator validation, and comment/blank-line treatment.
//
// The reference is canonical Git's
// [refs/packed-backend.c::next_record].
//
// [refs/packed-backend.c::next_record]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L886
func TestParsePackedRefs(t *testing.T) {
	t.Parallel()

	// SHA-1 fixture OIDs. Forty hex chars each so the parser's
	// length check passes.
	const (
		hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		hexC = "cccccccccccccccccccccccccccccccccccccccc"
		hexD = "dddddddddddddddddddddddddddddddddddddddd"
	)
	mkOID := func(t *testing.T, hex string) objfmt.SHA1Hash {
		t.Helper()

		h, err := objfmt.ParseSHA1Hex(hex)
		require.NoError(t, err)
		return h
	}

	type want struct {
		traits  packedTraits
		entries map[string]packedEntry[objfmt.SHA1Hash]
	}

	tests := []struct {
		name      string
		input     string
		want      want
		wantErr   bool
		wantErrIs error
	}{
		{
			name: "header_with_all_known_traits",
			input: "# pack-refs with: peeled fully-peeled sorted\n" +
				hexA + " refs/heads/main\n",
			want: want{
				traits: packedTraits{peeled: true, fullyPeeled: true, sorted: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name: "header_with_unknown_token_between_known",
			input: "# pack-refs with: peeled some-future-trait fully-peeled\n" +
				hexA + " refs/heads/main\n",
			want: want{
				traits: packedTraits{peeled: true, fullyPeeled: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name:  "header_missing_entirely_no_refs",
			input: "",
			want:  want{entries: map[string]packedEntry[objfmt.SHA1Hash]{}},
		},
		{
			name: "header_missing_with_refs_yields_zero_traits",
			input: hexA + " refs/heads/main\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			// Canonical Git pins the traits header to the very first
			// byte of the file: [refs/packed-backend.c:719] checks
			// `*snapshot->buf == '#'` and only consumes one line. A
			// `# pack-refs with:` line that follows ref lines is not a
			// header — it is treated as a body comment and the traits
			// remain at their zero value.
			//
			// [refs/packed-backend.c:719]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L719
			name: "header_after_ref_is_not_parsed_as_traits",
			input: hexA + " refs/heads/main\n" +
				"# pack-refs with: sorted\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				traits: packedTraits{},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			// A `# pack-refs with:` line on line 2 (after a ref on
			// line 1) is a body comment, not a traits header. The
			// traits stay at the zero value and the ref on line 1
			// registers normally.
			name: "header_on_line_two_is_body_comment",
			input: hexA + " refs/heads/main\n" +
				"# pack-refs with: fully-peeled\n",
			want: want{
				traits: packedTraits{},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name:  "single_ref_with_trailing_newline",
			input: hexA + " refs/heads/main\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			// Canonical Git tolerates a missing terminator on the last
			// line; `bufio.Scanner` yields it without one and the
			// parser must register the entry all the same.
			name:  "single_ref_no_trailing_newline",
			input: hexA + " refs/heads/main",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name: "two_refs_separated_by_LF",
			input: hexA + " refs/heads/main\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			// Every line — header, ref, peel — uses CRLF. The parser
			// strips the trailing `\r` so all three categories must
			// land identically to their LF-only cousins.
			name: "CRLF_on_header_ref_and_peel",
			input: "# pack-refs with: peeled fully-peeled\r\n" +
				hexA + " refs/heads/main\r\n" +
				hexC + " refs/tags/v1\r\n" +
				"^" + hexD + "\r\n",
			want: want{
				traits: packedTraits{peeled: true, fullyPeeled: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
					"refs/tags/v1": {
						oid:        mkOID(t, hexC),
						peeled:     mkOID(t, hexD),
						peelKnown:  true,
						fromPacked: true,
					},
				},
			},
		},
		{
			name: "peel_attaches_to_preceding_ref",
			input: hexC + " refs/tags/v1\n" +
				"^" + hexD + "\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/tags/v1": {
						oid:        mkOID(t, hexC),
						peeled:     mkOID(t, hexD),
						peelKnown:  true,
						fromPacked: true,
					},
				},
			},
		},
		{
			// Canonical Git's record iterator consumes one peel line
			// per record ([refs/packed-backend.c:952]). A second
			// `^<oid>` line is the start of the next record, which
			// `parse_oid_hex_algop` then rejects because `^` is not a
			// hex digit. The Go parser surfaces the same condition as
			// an `ErrCorruptObject`-wrapped error.
			//
			// [refs/packed-backend.c:952]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L952
			name: "double_peel_rejected",
			input: hexC + " refs/tags/v1\n" +
				"^" + hexD + "\n" +
				"^" + hexA + "\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "peel_without_preceding_ref_rejected",
			input:     "^" + hexD + "\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "tab_between_oid_and_name_rejected",
			input:     hexA + "\trefs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "double_space_between_oid_and_name_rejected",
			input:     hexA + "  refs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// `<oid> <tab>refs/...` — a single space separator
			// followed by a name beginning with a tab is rejected on
			// the `name[0] == '\t'` gate. Canonical `git update-ref`
			// writes no whitespace inside a ref name.
			name:      "leading_tab_on_name_rejected",
			input:     hexA + " \trefs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "empty_name_rejected",
			input:     hexA + " \n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// `oid` followed by no space at all (and no peel marker)
			// trips the missing-separator branch: `strings.Cut`
			// returns ok=false.
			name:      "no_separator_rejected",
			input:     hexA + "refs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// Comments after the header are tolerated and skipped;
			// the parser explicitly handles `#` lines in the body.
			// Ref names are kept in lexical order so the `sorted`
			// trait survives the on-the-fly verification — the case
			// targets comment handling, not sort enforcement.
			name: "body_comments_skipped",
			input: "# pack-refs with: sorted\n" +
				"# a body comment\n" +
				hexB + " refs/heads/feature\n" +
				"# another comment after a ref\n" +
				hexA + " refs/heads/main\n",
			want: want{
				traits: packedTraits{sorted: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name: "blank_lines_skipped",
			input: "\n" +
				"# pack-refs with: peeled\n" +
				"\n" +
				hexA + " refs/heads/main\n" +
				"\n" +
				hexC + " refs/tags/v1\n" +
				"^" + hexD + "\n" +
				"\n",
			want: want{
				traits: packedTraits{peeled: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
					"refs/tags/v1": {
						oid:        mkOID(t, hexC),
						peeled:     mkOID(t, hexD),
						peelKnown:  true,
						fromPacked: true,
					},
				},
			},
		},
		{
			name:      "short_oid_hex_rejected",
			input:     "deadbeef refs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name: "short_peel_hex_rejected",
			input: hexC + " refs/tags/v1\n" +
				"^deadbeef\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// Canonical `sort_snapshot` ([refs/packed-backend.c:380])
			// verifies the sort on-the-fly during record iteration: on
			// the first out-of-order pair the snapshot's `sorted` flag is
			// cleared. The Go parser must do the same so a corrupt or
			// hostile file claiming `sorted` cannot mislead downstream
			// short-circuits.
			//
			// [refs/packed-backend.c:380]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L380
			name: "sorted_trait_cleared_when_entries_out_of_order",
			input: "# pack-refs with: sorted\n" +
				hexA + " refs/heads/zebra\n" +
				hexB + " refs/heads/apple\n",
			want: want{
				traits: packedTraits{sorted: false},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/zebra": {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/apple": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			// Companion to the above: when entries are in order the trait
			// must remain set so callers may rely on it.
			name: "sorted_trait_preserved_when_entries_in_order",
			input: "# pack-refs with: sorted\n" +
				hexA + " refs/heads/apple\n" +
				hexB + " refs/heads/zebra\n",
			want: want{
				traits: packedTraits{sorted: true},
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/apple": {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/zebra": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},

		// Refname-format validation rows. Canonical Git's packed-refs
		// iterator runs `check_refname_format(name, REFNAME_ALLOW_ONELEVEL)`
		// on every record ([refs/packed-backend.c:938]) and marks
		// non-conforming names `REF_BAD_NAME | REF_ISBROKEN` so callers
		// see a sanitized empty OID rather than the corrupt input. For
		// our read-only library the equivalent is to refuse the file at
		// parse time: a downstream consumer that received a "valid" ref
		// pointing at a broken name would have no way to flag it. The
		// rules are taken from [refs.c::check_refname_component] and the
		// `refname_disposition` table on [refs.c:80].
		//
		// [refs/packed-backend.c:938]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L938
		// [refs.c::check_refname_component]: https://github.com/git/git/blob/v2.54.0/refs.c#L192
		// [refs.c:80]: https://github.com/git/git/blob/v2.54.0/refs.c#L80
		{
			name:      "refname_with_nul_byte_rejected",
			input:     hexA + " refs/heads/ma\x00in\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_control_char_rejected",
			input:     hexA + " refs/heads/ma\x01in\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_del_char_rejected",
			input:     hexA + " refs/heads/ma\x7fin\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// A space inside the refname is rejected. The grammar uses a
			// single ASCII space as the column separator, but a *second*
			// space inside the name still falls foul of the disposition
			// table (32 -> 4, "bad character"). The earlier
			// `double_space_between_oid_and_name_rejected` row exercises
			// the separator gate; this one exercises the refname check.
			name:      "refname_with_space_rejected",
			input:     hexA + " refs/heads/main bar\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_tab_rejected",
			input:     hexA + " refs/heads/main\tbar\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_tilde_rejected",
			input:     hexA + " refs/heads/foo~1\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_caret_rejected",
			input:     hexA + " refs/heads/foo^1\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_colon_rejected",
			input:     hexA + " refs/heads/foo:bar\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_question_rejected",
			input:     hexA + " refs/heads/foo?\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_asterisk_rejected",
			input:     hexA + " refs/heads/foo*\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_open_bracket_rejected",
			input:     hexA + " refs/heads/foo[1]\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_backslash_rejected",
			input:     hexA + " refs/heads/foo\\bar\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_starting_with_slash_rejected",
			input:     hexA + " /refs/heads/main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_ending_with_slash_rejected",
			input:     hexA + " refs/heads/main/\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_double_slash_rejected",
			input:     hexA + " refs/heads//main\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_dotdot_rejected",
			input:     hexA + " refs/heads/foo..bar\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_component_starting_with_dot_rejected",
			input:     hexA + " refs/heads/.hidden\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_component_ending_with_dot_lock_rejected",
			input:     hexA + " refs/heads/main.lock\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_with_at_brace_rejected",
			input:     hexA + " refs/heads/foo@{bar}\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_equals_at_rejected",
			input:     hexA + " @\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			name:      "refname_ending_with_dot_rejected",
			input:     hexA + " refs/heads/foo.\n",
			wantErr:   true,
			wantErrIs: ErrCorruptObject,
		},
		{
			// Canonical Git's on-disk validator (`check_refname_format`
			// with `REFNAME_ALLOW_ONELEVEL`) does not reject a leading
			// `-` in a component — that rejection lives in argv parsing
			// (`strbuf_check_branch_ref` and the option-name guards) and
			// does not gate stored refs. A `refs/heads/-foo` name in
			// `packed-refs` therefore parses cleanly here.
			name:  "refname_component_starting_with_dash_accepted",
			input: hexA + " refs/heads/-foo\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/-foo": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			// Single-component name. Canonical Git passes
			// `REFNAME_ALLOW_ONELEVEL` to `check_refname_format` from the
			// packed-refs iterator ([refs/packed-backend.c:938]), so
			// names like `HEAD` are tolerated even though
			// `git update-ref` would refuse to write them outside `refs/`.
			//
			// [refs/packed-backend.c:938]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L938
			name:  "refname_single_component_accepted",
			input: hexA + " HEAD\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"HEAD": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name:  "refname_with_subdir_accepted",
			input: hexA + " refs/heads/feature/sub-branch\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/heads/feature/sub-branch": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name:  "refname_remote_with_dots_accepted",
			input: hexA + " refs/tags/v1.0.0\n",
			want: want{
				entries: map[string]packedEntry[objfmt.SHA1Hash]{
					"refs/tags/v1.0.0": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parsePackedRefs[objfmt.SHA1Hash](strings.NewReader(tc.input))
			if tc.wantErr {
				require.Error(t, err)
				if tc.wantErrIs != nil {
					require.ErrorIs(t, err, tc.wantErrIs,
						"expected error to wrap %v, got %v", tc.wantErrIs, err)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.traits, got.traits)
			assert.Equal(t, tc.want.entries, got.refs)
		})
	}
}
