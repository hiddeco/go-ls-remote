package reftable

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// FuzzOpenReader feeds arbitrary bytes through [OpenReader] and the hot
// read paths to assert that no input causes a panic. Successful opens
// also exercise [Reader.IterRefs] and [Reader.FindRef] so block walking
// and decoding are covered alongside the constructor's header/trailer
// gates. Malformed inputs must surface as ordinary errors.
func FuzzOpenReader(f *testing.F) {
	root := filepath.Join("..", "..", "testdata", "reftable")

	// Seed the corpus with the happy-path fixtures so the fuzzer starts
	// from valid files and mutates outward; the corrupt and truncated
	// fixtures provide negative-shape seeds that already exercise the
	// header and trailer guards.
	seedDirs := []string{
		"single-sha1",
		"single-sha256",
		"with-index-sha1",
		"without-index-sha1",
	}
	for _, dir := range seedDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			f.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".ref") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, dir, e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(data)
		}
	}
	for _, p := range []string{
		"corrupt-trailer-sha1.ref",
		"truncated-sha1.ref",
	} {
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.ref")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// Exercise both algorithm dispatches: a fuzz input may have any
		// hash_id tag, and the typed reader rejects mismatches early.
		// Trying both keeps coverage on the decode path regardless of
		// which algo the mutated header declares.
		if r, err := OpenReader[objfmt.SHA1Hash](path); err == nil {
			fuzzExerciseReader(r)
			_ = r.Close()
		}
		if r, err := OpenReader[objfmt.SHA256Hash](path); err == nil {
			fuzzExerciseReader(r)
			_ = r.Close()
		}
	})
}

// fuzzExerciseReader walks IterRefs and FindRef on an opened reader so
// block-level decoding panics (out-of-bounds slices, varint overflows)
// surface even when the header and trailer are well-formed.
func fuzzExerciseReader[H objfmt.Hash](r *Reader[H]) {
	for rec, iterErr := range r.IterRefs() {
		if iterErr != nil {
			return
		}
		_, _, _ = r.FindRef(string(rec.Name))
	}
}

// FuzzOpenStack feeds arbitrary bytes as a `tables.list` manifest into
// [OpenStack] to assert that manifest parsing and the subsequent reader
// opens never panic. The named tables either resolve against the
// fuzz-temp directory (almost always missing, surfacing as fs errors)
// or get rejected by [parseTablesList].
func FuzzOpenStack(f *testing.F) {
	root := filepath.Join("..", "..", "testdata", "reftable")

	seed, err := os.ReadFile(filepath.Join(root, "stack-shadow-sha1", "tables.list"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(""))
	f.Add([]byte("\n"))
	f.Add([]byte("a\nb\n"))
	f.Add([]byte("a\n\nb\n"))
	f.Add([]byte("just-bad-bytes"))

	f.Fuzz(func(t *testing.T, manifest []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tables.list"), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		// Try both algorithms, same rationale as [FuzzOpenReader]: a
		// mutated manifest could point at a reftable of either algo.
		if s, err := OpenStack[objfmt.SHA1Hash](dir); err == nil {
			_ = s.Close()
		}
		if s, err := OpenStack[objfmt.SHA256Hash](dir); err == nil {
			_ = s.Close()
		}
	})
}
