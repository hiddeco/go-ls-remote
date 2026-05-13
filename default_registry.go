package lsremote

import (
	"github.com/hiddeco/go-ls-remote/transport"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// defaultRegistry returns a [transport.Registry] populated with only
// the HTTP transport, covering the `https` and `http` schemes. It is
// the registry [Dial] falls back to when the caller omits
// [WithTransports]. Callers who need SSH, git, or file schemes must
// build their own [transport.Registry] and pass it via
// [WithTransports].
//
// Each call allocates a fresh [transport.Registry] so distinct
// [Dial]s never observe each other's [transport.Registry.Register]
// writes; the helper has no package-level state.
//
// The default registry is constructed here rather than inside the
// `transport` package to keep the dependency direction one-way: the
// scheme-specific packages (`transport/http`, future `transport/ssh`,
// ...) depend on `transport`, never the reverse. Anchoring the
// default in the root `lsremote` package — which already imports both
// — is the only place that avoids an import cycle while keeping the
// HTTP-only default decision recorded in code.
func defaultRegistry() *transport.Registry {
	return transport.NewRegistry(httpt.New())
}
