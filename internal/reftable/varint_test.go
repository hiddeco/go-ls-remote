package reftable

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_decodeVarint(t *testing.T) {
	t.Parallel()
	t.Run("known_values", func(t *testing.T) {
		t.Parallel()
		// Encodings hand-derived from the canonical formula in
		// [reftable.adoc § Varint encoding]:
		//
		//   val = buf[0] & 0x7f
		//   while (buf[i] & 0x80) {
		//       i++; val = ((val + 1) << 7) | (buf[i] & 0x7f)
		//   }
		//
		// The `+1` per continuation byte is the off-by-one that
		// distinguishes reftable's varint from the protobuf-style
		// encoding; it removes redundant zero encodings.
		//
		// [reftable.adoc § Varint encoding]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#varint-encoding
		cases := []struct {
			name  string
			input []byte
			want  uint64
			n     int
		}{
			{"zero", []byte{0x00}, 0, 1},
			{"max_one_byte", []byte{0x7f}, 0x7f, 1},
			{"first_two_byte", []byte{0x80, 0x00}, 0x80, 2},
			{"two_byte_low", []byte{0x80, 0x01}, 0x81, 2},
			{"two_byte_max", []byte{0xff, 0x7f}, 0x407f, 2},
			{"three_byte_4080", []byte{0x80, 0x80, 0x00}, 0x4080, 3},
			{"four_byte_204080", []byte{0x80, 0x80, 0x80, 0x00}, 0x204080, 4},
			{"four_byte_208080", []byte{0x80, 0x81, 0x80, 0x00}, 0x208080, 4},
			{"trailing_bytes_ignored", []byte{0x05, 0xff, 0xff}, 5, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				got, n, err := decodeVarint(tc.input)
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				assert.Equal(t, tc.n, n)
			})
		}
	})

	t.Run("empty_buffer_rejected", func(t *testing.T) {
		t.Parallel()
		_, _, err := decodeVarint(nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncatedRecord, "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("truncated_continuation", func(t *testing.T) {
		t.Parallel()
		// 0x80 sets the continuation bit but no further byte follows.
		_, _, err := decodeVarint([]byte{0x80})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncatedRecord, "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("overflow_eleven_bytes", func(t *testing.T) {
		t.Parallel()
		// Eleven bytes with the high bit on every byte: a malformed
		// stream that would never terminate. 11 > varintMaxBytes.
		buf := []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00}
		_, _, err := decodeVarint(buf)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrVarintOverflow, "want ErrVarintOverflow, got %v", err)
	})

	t.Run("value_overflow", func(t *testing.T) {
		t.Parallel()
		// Ten 0xff continuation bytes terminated by 0x7f yields a
		// running value that steps past uint64; canonical Git rejects
		// this branch via the same shift check.
		buf := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
		_, _, err := decodeVarint(buf)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrVarintOverflow, "want ErrVarintOverflow, got %v", err)
	})
}
