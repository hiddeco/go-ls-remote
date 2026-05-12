package lsremote_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// ExampleDial opens one session and issues several discovery commands
// against it. Reusing a single session amortises the handshake — useful
// when a caller wants both a ref list and per-object metadata from the
// same remote.
//
// A [lsremote.Session] is safe for concurrent use only when the
// underlying transport multiplexes commands onto independent requests.
// The HTTP transport satisfies that; SSH, `git://`, and `file://` do
// not — callers using a non-HTTP transport must serialise method calls
// externally.
func ExampleDial() {
	ctx := context.Background()
	session, err := lsremote.Dial(ctx,
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	caps := session.Capabilities()
	fmt.Println("protocol     ", caps.Version)
	fmt.Println("server agent ", caps.Agent)
	fmt.Println("object format", caps.ObjectFormat)

	heads, err := session.ListRefs(ctx, lsremote.RefsRequest{
		Prefixes: []string{"refs/heads/"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("branches:", len(heads))
}

// ExampleSession_Refs walks the iterator returned by
// [lsremote.Session.Refs] and breaks out early. The library wraps the
// iterator so the connection state stays consistent on early `break`:
// the response is drained behind the scenes and the session remains
// usable for subsequent commands — load-bearing on the SSH, `git://`,
// and `file://` transports where one byte stream carries every command.
func ExampleSession_Refs() {
	ctx := context.Background()
	session, err := lsremote.Dial(ctx,
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	seq, err := session.Refs(ctx, lsremote.RefsRequest{
		Prefixes: []string{"refs/tags/"},
		Peel:     true,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Take the first ten tags only.
	n := 0
	for ref, err := range seq {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(ref.Name)
		if n++; n == 10 {
			break
		}
	}
}

// ExampleSession_ObjectInfo asks the server for the payload size of a
// handful of objects without fetching them — useful for supply-chain
// sizing, mirror estimates, or release-asset audits. `object-info` is
// a v2-only command; sessions that negotiated v0 or v1, or v2 servers
// that did not advertise the command, surface
// [lsremote.ErrUnsupportedProtocol].
//
// [lsremote.ObjectInfo.Size] is `-1` when the size was not requested
// or the server elided it; a real on-disk object can legitimately have
// a size of zero (an empty blob), so the negative sentinel is the only
// unambiguous "absent" marker.
func ExampleSession_ObjectInfo() {
	ctx := context.Background()
	session, err := lsremote.Dial(ctx,
		"https://github.com/octocat/Hello-World.git")
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	infos, err := session.ObjectInfo(ctx, []string{
		"7fd1a60b01f91b314f59955a4e4d4e80d8edf11d",
		"553c2077f0edc3d5dc5d17262f6aa498e69d6f8e",
	}, lsremote.ObjectInfoRequest{Size: true})
	if err != nil {
		// A v2 server without object-info advertised is a common case
		// on hosts that only ship ls-refs and fetch; degrade gracefully.
		if errors.Is(err, lsremote.ErrUnsupportedProtocol) {
			fmt.Println("server does not support object-info")
			return
		}
		log.Fatal(err)
	}
	for _, info := range infos {
		fmt.Printf("%s %d bytes\n", info.Hash, info.Size)
	}
}
