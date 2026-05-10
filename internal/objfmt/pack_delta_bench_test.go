package objfmt

import "testing"

// BenchmarkPack_ReadDeltaHeader characterises the per-hop cost the
// `internal/objstore` delta walker pays when chasing an `OFS_DELTA`
// or `REF_DELTA` chain: one zlib reader init, one short inflate, and
// two little-endian 7-bit varint decodes per `delta.h` lines 85-102
// (`get_delta_hdr_size`). The walker calls `Pack.ReadDeltaHeader`
// once per delta hop, so this is the hot path.
//
// `ofs-delta.pack` places the OFS_DELTA at offset 207 with `BodyAt`
// at 210 (header `0x69`, offset varint `0x80 0x43`). The deltified
// payload's source/target sizes encode as `0xc5 0x89 0x01` /
// `0xc0 0x89 0x01` — three-byte varints decoding to 17605 and 17600,
// representative of the medium-sized base + same-size target shape
// produced by the `git pack-objects` heuristic for blob deltas.
func BenchmarkPack_ReadDeltaHeader(b *testing.B) {
	p, err := OpenPack(benchPackFixture("ofs-delta.pack"), SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = p.Close() })
	const bodyAt = int64(210)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := p.ReadDeltaHeader(bodyAt); err != nil {
			b.Fatal(err)
		}
	}
}
