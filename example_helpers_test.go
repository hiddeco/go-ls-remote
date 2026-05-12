package lsremote_test

import (
	"context"
	"fmt"
	"log"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// ExampleTags streams a remote repository's tags through an iterator.
// [lsremote.Ref.Peeled] carries the commit OID an annotated tag points
// to; lightweight tags leave [lsremote.Ref.Peeled] empty and
// [lsremote.Ref.Hash] already points directly at the commit.
func ExampleTags() {
	seq, err := lsremote.Tags(context.Background(),
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	for ref, err := range seq {
		if err != nil {
			log.Fatal(err)
		}
		commit := ref.Hash
		if ref.Peeled != "" {
			commit = ref.Peeled
		}
		fmt.Printf("%s\t%s\n", commit, ref.Name)
	}
}

// ExampleListRefs collects every advertised ref into a slice. Use this
// when the caller wants the slice form rather than the iterator
// returned by [lsremote.Refs]; the two share the same underlying
// `ls-refs` exchange on a v2 server.
//
// [lsremote.RefsRequest.Symrefs] asks the server to disclose the target
// of symbolic references (`HEAD`, `refs/remotes/origin/HEAD`, ...);
// [lsremote.Ref.Symref] is empty for non-symbolic refs.
func ExampleListRefs() {
	refs, err := lsremote.ListRefs(context.Background(),
		"https://github.com/octocat/Hello-World.git",
		lsremote.RefsRequest{Symrefs: true})
	if err != nil {
		log.Fatal(err)
	}
	for _, ref := range refs {
		if ref.Symref != "" {
			fmt.Printf("%s -> %s\n", ref.Name, ref.Symref)
			continue
		}
		fmt.Printf("%s %s\n", ref.Hash, ref.Name)
	}
}

// ExampleExists reports whether a remote repository is reachable. A
// missing-repository condition collapses to (false, nil), so callers
// can distinguish "not there" from "transport failure" without
// inspecting the error. Authentication failures, DNS errors, and
// other transport-level problems still propagate as the error return.
func ExampleExists() {
	ok, err := lsremote.Exists(context.Background(),
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	if ok {
		fmt.Println("found")
	} else {
		fmt.Println("missing")
	}
}
