package ssht_test

import (
	"context"
	"fmt"
	"log"

	"golang.org/x/crypto/ssh"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/transport"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
)

// ExampleAgent authenticates over SSH using keys held by `ssh-agent`,
// the same path `git fetch` takes for most interactive users. The
// host-key callback is mandatory: a [ssht.Transport] with no callback
// rejects every dial at host-key verification.
//
// In production, build the [ssh.HostKeyCallback] from
// `~/.ssh/known_hosts` via
// [golang.org/x/crypto/ssh/knownhosts]; the example uses
// [ssh.InsecureIgnoreHostKey] only so it stays self-contained.
func ExampleAgent() {
	// Replace with knownhosts.New(filepath.Join(home, ".ssh/known_hosts"))
	// in production code.
	hostKey := ssh.InsecureIgnoreHostKey()

	sshTransport := ssht.New(
		ssht.WithAuth(ssht.Agent()),
		ssht.WithKnownHosts(hostKey),
	)
	reg := transport.NewRegistry(sshTransport)

	refs, err := lsremote.ListRefs(context.Background(),
		"ssh://git@github.com/octocat/Hello-World.git",
		lsremote.RefsRequest{},
		lsremote.WithTransports(reg))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(refs), "refs")
}
