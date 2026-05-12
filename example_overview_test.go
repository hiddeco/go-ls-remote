package lsremote_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
)

// Example_defaultBranch resolves the canonical name of a remote
// repository's default branch — the target of HEAD — without cloning
// or hitting the filesystem. The returned name is the full ref path
// (`refs/heads/main`, `refs/heads/master`, ...), matching the form a
// caller would write into a `git fetch` invocation.
func Example_defaultBranch() {
	branch, err := lsremote.DefaultBranch(context.Background(),
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(branch)
}

// Example_pollLatestCommit retrieves the current commit hash of a
// specific branch — the building block for "is there anything new to
// deploy?" polling, image-automation triggers, and similar workflows.
// The v2 wire applies the prefix filter server-side, so the request
// is cheap even on remotes with millions of refs.
func Example_pollLatestCommit() {
	ctx := context.Background()
	refs, err := lsremote.ListRefs(ctx,
		"https://github.com/octocat/Hello-World.git",
		lsremote.RefsRequest{Prefixes: []string{"refs/heads/master"}})
	if err != nil {
		log.Fatal(err)
	}
	for _, ref := range refs {
		fmt.Printf("%s\t%s\n", ref.Hash, ref.Name)
	}
}

// Example_handleErrors illustrates the package's sentinel error model.
// Every transport-level failure flows through [lsremote.ProtocolError]
// and bridges to one of the public sentinels via [errors.Is], so
// callers never have to branch on transport-specific error types. Use
// [errors.As] to recover diagnostic fields such as the HTTP status code
// or a server-supplied excerpt.
//
// The example dials a non-existent local path so the output is
// deterministic; the same error-handling shape applies to every
// transport.
func Example_handleErrors() {
	// The package default registers HTTPS/HTTP only; opt into the file
	// transport explicitly. See [lsremote.WithTransports] for the full
	// composition pattern.
	reg := transport.NewRegistry(filet.New())

	_, err := lsremote.Dial(context.Background(),
		"file:///does/not/exist",
		lsremote.WithTransports(reg))

	var perr *lsremote.ProtocolError
	switch {
	case errors.Is(err, lsremote.ErrNotFound):
		fmt.Println("not found")
	case errors.Is(err, lsremote.ErrAuthRequired):
		fmt.Println("auth required")
	case errors.Is(err, lsremote.ErrAuthFailed):
		fmt.Println("auth failed")
	case errors.As(err, &perr):
		fmt.Printf("protocol error at %s: %v\n", perr.Op, perr.Err)
	case err != nil:
		fmt.Printf("other error: %v\n", err)
	}
	// Output: not found
}
