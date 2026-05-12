package httpt_test

import (
	"context"
	"fmt"
	"log"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/transport"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// ExampleBasic wires HTTP Basic authentication for every dial routed
// through the HTTP transport. Pair [httpt.Basic] with [httpt.Static]
// when the same credential applies to every host; implement
// [httpt.CredentialResolver] directly for per-host credentials, OAuth
// refresh flows, or credential-helper integrations.
//
// For GitHub-style personal access tokens, pass the token as either
// the password (with any non-empty user) or via [httpt.Bearer].
func ExampleBasic() {
	creds := httpt.Static(httpt.Basic("user", "pat-or-password"))

	reg := transport.NewRegistry(
		httpt.New(httpt.WithCredentials(creds)),
	)

	refs, err := lsremote.ListRefs(context.Background(),
		"https://example.com/private/repo.git",
		lsremote.RefsRequest{},
		lsremote.WithTransports(reg))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(refs), "refs")
}

// ExampleNetrc resolves credentials from `~/.netrc`, the same file the
// canonical Git client consults. Hosts not listed in the file fall
// through to anonymous access; a stricter "must authenticate" policy
// can be layered on by wrapping [httpt.Netrc] in a custom
// [httpt.CredentialResolver].
func ExampleNetrc() {
	reg := transport.NewRegistry(
		httpt.New(httpt.WithCredentials(httpt.Netrc())),
	)

	refs, err := lsremote.ListRefs(context.Background(),
		"https://example.com/private/repo.git",
		lsremote.RefsRequest{},
		lsremote.WithTransports(reg))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(len(refs), "refs")
}
