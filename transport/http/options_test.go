package httpt

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_zeroValueDefaults(t *testing.T) {
	t.Parallel()
	tr := New()
	require.NotNil(t, tr)

	assert.Nil(t, tr.client, "client is nil; resolved to http.DefaultClient at Open time")
	assert.Nil(t, tr.creds, "creds is nil; means no credentials")
	assert.Empty(t, tr.userAgent, "userAgent default is empty; caller decides at Open time")
	assert.Equal(t, FollowRedirectsInitial, tr.followRedirects,
		"followRedirects zero value is Initial per Documentation/config/http.adoc:359-365")
	assert.Equal(t, 0, tr.maxRedirects,
		"maxRedirects zero value defers default resolution (10) to Open time")
}

func TestWithClient(t *testing.T) {
	t.Parallel()
	want := &http.Client{}
	tr := New(WithClient(want))

	assert.Same(t, want, tr.client, "WithClient stores the *http.Client verbatim")
}

func TestWithClient_nilPermitted(t *testing.T) {
	t.Parallel()
	// Passing nil is a documented "use http.DefaultClient" signal; the
	// constructor must accept it without panicking.
	tr := New(WithClient(nil))
	assert.Nil(t, tr.client)
}

func TestWithCredentials(t *testing.T) {
	t.Parallel()
	resolverCalls := 0
	want := credentialResolverFunc(func(_ context.Context, _ *url.URL) (Credentials, error) {
		resolverCalls++
		return nil, nil
	})

	tr := New(WithCredentials(want))

	require.NotNil(t, tr.creds, "WithCredentials stores the resolver")
	_, err := tr.creds.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	assert.Equal(t, 1, resolverCalls, "stored resolver is the one passed in")
}

func TestWithCredentials_nilPermitted(t *testing.T) {
	t.Parallel()
	tr := New(WithCredentials(nil))
	assert.Nil(t, tr.creds, "WithCredentials(nil) means no auth")
}

func TestWithUserAgent(t *testing.T) {
	t.Parallel()
	tr := New(WithUserAgent("test-agent/1.0"))
	assert.Equal(t, "test-agent/1.0", tr.userAgent)
}

func TestWithFollowRedirects(t *testing.T) {
	t.Parallel()
	tr := New(WithFollowRedirects(FollowRedirectsAlways))
	assert.Equal(t, FollowRedirectsAlways, tr.followRedirects)

	tr = New(WithFollowRedirects(FollowRedirectsNever))
	assert.Equal(t, FollowRedirectsNever, tr.followRedirects)
}

func TestWithFollowRedirects_zeroValueIsInitial(t *testing.T) {
	t.Parallel()
	// The zero value of FollowRedirects MUST be Initial per
	// Documentation/config/http.adoc:359-365.
	assert.Equal(t, FollowRedirectsInitial, FollowRedirects(0),
		"FollowRedirectsInitial must be the zero value (iota 0)")
	assert.Equal(t, FollowRedirectsInitial, New().followRedirects)
}

func TestWithMaxRedirects(t *testing.T) {
	t.Parallel()
	tr := New(WithMaxRedirects(5))
	assert.Equal(t, 5, tr.maxRedirects)

	// Negative values are stored verbatim here; normalisation to 0
	// happens at Open time. This test pins the storage contract so
	// future readers know what to normalise.
	tr = New(WithMaxRedirects(-1))
	assert.Equal(t, -1, tr.maxRedirects,
		"WithMaxRedirects stores negatives verbatim; Open normalises later")
}

func TestNew_appliesAllOptions(t *testing.T) {
	t.Parallel()
	client := &http.Client{}
	resolver := credentialResolverFunc(func(_ context.Context, _ *url.URL) (Credentials, error) {
		return nil, nil
	})

	tr := New(
		WithClient(client),
		WithCredentials(resolver),
		WithUserAgent("ua/1"),
		WithFollowRedirects(FollowRedirectsAlways),
		WithMaxRedirects(7),
	)

	assert.Same(t, client, tr.client)
	assert.NotNil(t, tr.creds)
	assert.Equal(t, "ua/1", tr.userAgent)
	assert.Equal(t, FollowRedirectsAlways, tr.followRedirects)
	assert.Equal(t, 7, tr.maxRedirects)
}

func TestFollowRedirects_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   FollowRedirects
		want string
	}{
		{"initial", FollowRedirectsInitial, "initial"},
		{"always", FollowRedirectsAlways, "always"},
		{"never", FollowRedirectsNever, "never"},
		{"unknown", FollowRedirects(99), "unknown(99)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.in.String())
		})
	}
}

// credentialResolverFunc is a lightweight test adapter that turns a
// closure into a [CredentialResolver]. It stays in the test file so
// the production package surface is not polluted.
type credentialResolverFunc func(ctx context.Context, u *url.URL) (Credentials, error)

func (f credentialResolverFunc) Resolve(ctx context.Context, u *url.URL) (Credentials, error) {
	return f(ctx, u)
}
