package lsremote_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	gitt "github.com/hiddeco/go-ls-remote/transport/git"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// ExampleWithTransports composes a registry covering schemes beyond
// the HTTPS-only default. SSH and `git://` are intentionally opt-in so
// a misconfigured caller cannot accidentally reach a non-HTTPS remote;
// the same pattern adds them once the per-transport options
// (credentials, host-key callback) have been wired in. See the `ssht`
// and `gitt` package examples for the per-transport setup.
func ExampleWithTransports() {
	reg := transport.NewRegistry(
		httpt.New(),
		gitt.New(),
		filet.New(),
	)

	ctx := context.Background()
	refs, err := lsremote.ListRefs(ctx, "git://example.com/repo.git",
		lsremote.RefsRequest{},
		lsremote.WithTransports(reg))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(refs), "refs")
}

// ExampleWithProtocol pins protocol-version negotiation. Auto-
// negotiation (the default) prefers v2 and falls back to v0; pin v0
// only when the caller knows the remote does not understand v2, or to
// reproduce a historical interaction.
func ExampleWithProtocol() {
	refs, err := lsremote.ListRefs(context.Background(),
		"https://example.com/git/legacy.git",
		lsremote.RefsRequest{},
		lsremote.WithProtocol(lsremote.ProtocolV0))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(refs), "refs")
}

// ExampleWithTracer captures pkt-line, HTTP, and command lifecycle
// events through a [trace.Tracer]. [trace.NewWriterTracer] dumps a
// human-readable summary to an [io.Writer], comparable to canonical
// Git's `GIT_TRACE_PACKET=` output; implement [trace.Tracer] directly
// for structured or machine-readable consumption.
func ExampleWithTracer() {
	tracer := trace.NewWriterTracer(os.Stderr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := lsremote.ListRefs(ctx,
		"https://github.com/octocat/Hello-World.git",
		lsremote.RefsRequest{},
		lsremote.WithTracer(tracer))
	if err != nil {
		log.Fatal(err) //nolint:gocritic // example doc pattern: log.Fatal mirrors typical caller code
	}
}
