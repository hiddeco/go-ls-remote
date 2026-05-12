package wire

import (
	"strconv"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// HTTPProtocolHeader returns the value of the `Git-Protocol` HTTP
// request header that the client sends to a smart-HTTP server during
// reference discovery.
//
// A nil v means "auto-negotiate" — the client advertises the highest
// version it supports and lets the server pick. Canonical Git
// announces `version=2` by default in this case (see
// [remote-curl.c::http_options] and [protocol.c::get_protocol_version_config]),
// so the auto path returns `version=2`. A non-nil v pins the request
// to that exact version: `version=0`, `version=1`, or `version=2`.
//
// [remote-curl.c::http_options]: https://github.com/git/git/blob/v2.54.0/remote-curl.c#L476
// [protocol.c::get_protocol_version_config]: https://github.com/git/git/blob/v2.54.0/protocol.c#L21
func HTTPProtocolHeader(v *transport.ProtocolVersion) string {
	if v == nil {
		return "version=2"
	}
	return "version=" + strconv.Itoa(int(*v))
}

// WriteStreamRequest emits the single pkt-line that initiates a Git
// session over a stream transport (git-daemon and SSH). The encoding
// matches [connect.c::git_connect_git lines 1288-1298] in canonical
// Git, and the on-wire grammar is documented in [gitprotocol-pack.adoc
// §"Extra Parameters"]:
//
//	git-proto-request = request-command SP pathname NUL
//	                    [ host-parameter NUL ] [ NUL extra-parameters ]
//	host-parameter    = "host=" hostname [ ":" port ]
//	extra-parameter   = 1*( %x01-ff ) NUL
//
// This library is read-only, so the request-command is always
// `git-upload-pack`.
//
// The payload carries no trailing LF; canonical Git strips one if
// present (see [daemon.c:752-754]), so omitting it is the safe shape.
//
// The version trailer is conditional on the requested protocol being
// strictly greater than zero: canonical Git's `version > 0` guard at
// [connect.c:1294] means v0 is signalled by the *absence* of a trailer,
// not by `version=0`. A nil v auto-negotiates to v2, matching the
// transport layer's `OpenOptions.PreferredProtocol == nil` convention.
//
// The host-parameter mirrors the authority verbatim. `parse_connect_url`
// at [connect.c:1117] retains the bracketed form of an IPv6 literal
// (`removebrackets=0`), and `git_connect_git` at [connect.c:1267] then
// emits that exact string as `target_host`. So an IPv6 host with a
// port becomes `host=[<addr>]:<port>`; a bare IPv6 host with no port
// becomes `host=<addr>`. A regular hostname with port becomes
// `host=<host>:<port>`.
//
// [connect.c::git_connect_git lines 1288-1298]: https://github.com/git/git/blob/v2.54.0/connect.c#L1288-L1298
// [gitprotocol-pack.adoc §"Extra Parameters"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#extra-parameters
// [daemon.c:752-754]: https://github.com/git/git/blob/v2.54.0/daemon.c#L752-L754
// [connect.c:1294]: https://github.com/git/git/blob/v2.54.0/connect.c#L1294
// [connect.c:1117]: https://github.com/git/git/blob/v2.54.0/connect.c#L1117
// [connect.c:1267]: https://github.com/git/git/blob/v2.54.0/connect.c#L1267
func WriteStreamRequest(w *pktline.Writer, u *transport.URL, v *transport.ProtocolVersion) error {
	var b strings.Builder
	b.WriteString("git-upload-pack ")
	b.WriteString(u.Path)
	b.WriteByte(0)
	b.WriteString("host=")
	b.WriteString(hostParameter(u))
	b.WriteByte(0)

	version := transport.ProtocolV2
	if v != nil {
		version = *v
	}
	if version > 0 {
		b.WriteByte(0)
		b.WriteString("version=")
		b.WriteString(strconv.Itoa(int(version)))
		b.WriteByte(0)
	}

	return w.WritePacket([]byte(b.String()))
}

// hostParameter returns the value that follows `host=` in the
// host-parameter field. With no port set the bare host is returned;
// with a port set an IPv6 literal is rebracketed to disambiguate the
// host/port colon, matching the bracketed form `parse_connect_url`
// preserves at [connect.c:1117].
//
// [connect.c:1117]: https://github.com/git/git/blob/v2.54.0/connect.c#L1117
func hostParameter(u *transport.URL) string {
	if u.Port == "" {
		return u.Host
	}
	if strings.Contains(u.Host, ":") {
		return "[" + u.Host + "]:" + u.Port
	}
	return u.Host + ":" + u.Port
}
