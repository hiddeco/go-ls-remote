package objstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParsePackedRefs pins down the parser's contract directly, free of
// the loose-refs fixture machinery that exercises it indirectly. The
// table covers header-line traits, body line-ending tolerance, peel
// handling, separator validation, and comment/blank-line treatment.
//
// The reference is canonical Git's
// `refs/packed-backend.c::parse_packed_refs_line`.
func TestParsePackedRefs(t *testing.T) {
	// SHA-1 fixture OIDs. Forty hex chars each so the parser's
	// length check passes.
	const (
		hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		hexC = "cccccccccccccccccccccccccccccccccccccccc"
		hexD = "dddddddddddddddddddddddddddddddddddddddd"
	)
	mkOID := func(t *testing.T, hex string) objfmt.Hash {
		t.Helper()
		h, err := objfmt.ParseHex(hex, objfmt.SHA1)
		require.NoError(t, err)
		return h
	}

	type want struct {
		traits  packedTraits
		entries map[string]packedEntry
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
				entries: map[string]packedEntry{
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
				entries: map[string]packedEntry{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name:  "header_missing_entirely_no_refs",
			input: "",
			want:  want{entries: map[string]packedEntry{}},
		},
		{
			name: "header_missing_with_refs_yields_zero_traits",
			input: hexA + " refs/heads/main\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				entries: map[string]packedEntry{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			// The parser treats the first `#` line it sees as the
			// traits header regardless of position: blank lines and
			// in-body refs do not flip `headerSeen`. A `#` line on
			// line 1 is parsed as traits; a `#` line that follows a
			// ref line is *also* parsed as traits because the parser
			// does not pin the header to line 1. This row pins down
			// the actual behavior so a tightening (e.g. requiring the
			// header on line 1) becomes a visible regression.
			name: "header_after_ref_is_still_parsed_as_traits",
			input: hexA + " refs/heads/main\n" +
				"# pack-refs with: sorted\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				traits: packedTraits{sorted: true},
				entries: map[string]packedEntry{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
				},
			},
		},
		{
			name:  "single_ref_with_trailing_newline",
			input: hexA + " refs/heads/main\n",
			want: want{
				entries: map[string]packedEntry{
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
				entries: map[string]packedEntry{
					"refs/heads/main": {oid: mkOID(t, hexA), fromPacked: true},
				},
			},
		},
		{
			name: "two_refs_separated_by_LF",
			input: hexA + " refs/heads/main\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				entries: map[string]packedEntry{
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
				entries: map[string]packedEntry{
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
				entries: map[string]packedEntry{
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
			// The parser does not detect a duplicate peel: the second
			// `^<oid>` line silently overwrites the first. Pinned
			// down here so a future tightening that rejects the
			// duplicate (matching canonical Git's stricter readers)
			// surfaces as a visible test failure.
			name: "double_peel_second_overwrites_first",
			input: hexC + " refs/tags/v1\n" +
				"^" + hexD + "\n" +
				"^" + hexA + "\n",
			want: want{
				entries: map[string]packedEntry{
					"refs/tags/v1": {
						oid:        mkOID(t, hexC),
						peeled:     mkOID(t, hexA),
						peelKnown:  true,
						fromPacked: true,
					},
				},
			},
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
			name: "body_comments_skipped",
			input: "# pack-refs with: sorted\n" +
				"# a body comment\n" +
				hexA + " refs/heads/main\n" +
				"# another comment after a ref\n" +
				hexB + " refs/heads/feature\n",
			want: want{
				traits: packedTraits{sorted: true},
				entries: map[string]packedEntry{
					"refs/heads/main":    {oid: mkOID(t, hexA), fromPacked: true},
					"refs/heads/feature": {oid: mkOID(t, hexB), fromPacked: true},
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
				entries: map[string]packedEntry{
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePackedRefs(strings.NewReader(tc.input), objfmt.SHA1)
			if tc.wantErr {
				require.Error(t, err)
				if tc.wantErrIs != nil {
					assert.True(t, errors.Is(err, tc.wantErrIs),
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
