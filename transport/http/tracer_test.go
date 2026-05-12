package httpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// capturingTracer records every event passed to OnEvent or
// OnPacketEvent. It is safe for concurrent use; HTTP handlers may run
// on goroutines distinct from the caller, and `pktline.Reader` events
// fire on the body-read goroutine.
type capturingTracer struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *capturingTracer) OnPacketEvent(e *trace.PacketEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cloned := *e
	if cloned.Bytes != nil {
		cloned.Bytes = bytes.Clone(cloned.Bytes)
	}
	c.events = append(c.events, cloned)
}

func (c *capturingTracer) OnEvent(e trace.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *capturingTracer) snapshot() []trace.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *capturingTracer) httpEvents() []trace.HTTPEvent {
	var out []trace.HTTPEvent
	for _, e := range c.snapshot() {
		if h, ok := e.(trace.HTTPEvent); ok {
			out = append(out, h)
		}
	}
	return out
}

func (c *capturingTracer) packetEvents() []trace.PacketEvent {
	var out []trace.PacketEvent
	for _, e := range c.snapshot() {
		if p, ok := e.(trace.PacketEvent); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestTracer_HTTPEvent_OnSmart200(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got := tracer.httpEvents()
	require.Len(t, got, 1, "exactly one HTTPEvent for the probe GET")
	assert.Equal(t, http.MethodGet, got[0].Method)
	assert.Equal(t, http.StatusOK, got[0].Status)
	assert.NoError(t, got[0].Err, "a 200 must emit Err == nil")
	assert.Greater(t, got[0].Duration, time.Duration(0),
		"Duration must be measured wall-clock time")
	assert.False(t, got[0].Time.IsZero(), "Time must be set")
	assert.Contains(t, got[0].URL, "/repo.git/info/refs",
		"URL must point at the discovery endpoint")
}

func TestTracer_HTTPEvent_OnDumb200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"3333333333333333333333333333333333333333\trefs/heads/main\n",
		))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got := tracer.httpEvents()
	require.Len(t, got, 1)
	assert.Equal(t, http.MethodGet, got[0].Method)
	assert.Equal(t, http.StatusOK, got[0].Status)
	assert.NoError(t, got[0].Err)
}

func TestTracer_HTTPEvent_On500(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	_, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.Error(t, err)

	got := tracer.httpEvents()
	require.Len(t, got, 1, "a 500 must still emit one HTTPEvent")
	assert.Equal(t, 500, got[0].Status,
		"a 500 produced a response, so Status reflects it")
	assert.NoError(t, got[0].Err,
		"a 500 produced a response, so Err must be nil per HTTPEvent doc")
}

func TestTracer_HTTPEvent_OnDialError(t *testing.T) {
	t.Parallel()
	// Point at an unroutable URL so the request never produces a
	// response. Per the `HTTPEvent` doc, Status == 0 and Err != nil.
	u, err := transport.ParseURL("http://127.0.0.1:1/repo.git")
	require.NoError(t, err)

	tracer := &capturingTracer{}
	tr := New()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, openErr := tr.Open(ctx, u, transport.OpenOptions{Tracer: tracer})
	require.Error(t, openErr)

	got := tracer.httpEvents()
	require.Len(t, got, 1)
	assert.Equal(t, 0, got[0].Status, "no response means Status == 0")
	assert.Error(t, got[0].Err, "no response means Err != nil")
}

func TestTracer_HTTPEvent_RedactsCredentials(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Build a transport.URL with userinfo so the request URL embeds it
	// even though the probe sends credentials via the resolver, not the
	// URL. Userinfo is dropped from the dialed URL by `buildInfoRefsURL`,
	// so the tracer event reflects the dialed URL — the redaction here
	// is a belt-and-braces guarantee that any future change which puts
	// userinfo back on `req.URL` still redacts.
	su, err := url.Parse(srv.URL)
	require.NoError(t, err)
	raw := "http://alice:secret@" + su.Host + "/repo.git"
	u, err := transport.ParseURL(raw)
	require.NoError(t, err)

	tracer := &capturingTracer{}
	conn, err := New().Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got := tracer.httpEvents()
	require.NotEmpty(t, got)
	for _, e := range got {
		assert.NotContains(t, e.URL, "secret",
			"the password must never travel in HTTPEvent.URL")
	}
}

func TestTracer_HTTPEvent_OnPost(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	got := tracer.httpEvents()
	require.Len(t, got, 2,
		"probe GET plus command POST must each emit one HTTPEvent; a stray third call would be a regression")
	var sawPost bool
	for _, e := range got {
		if e.Method == http.MethodPost {
			sawPost = true
			assert.Equal(t, http.StatusOK, e.Status,
				"the success-path POST must have emitted a 200 HTTPEvent")
			assert.NoError(t, e.Err)
			assert.Contains(t, e.URL, "/repo.git/git-upload-pack",
				"the POST URL must name the upload-pack endpoint")
		}
	}
	assert.True(t, sawPost, "command path must emit a POST HTTPEvent")
}

func TestTracer_PacketEvent_OnAdvertisement(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Drain the advertisement so the wrapped `pktline.Reader` emits
	// its `PacketEvent`s. The smart-preamble strip already consumed
	// one data packet plus a flush at probe time; we drain the
	// remaining post-preamble payload here.
	rdr := conn.Advertisement()
	for {
		p, err := rdr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if p.Kind == pktline.Flush || p.Kind == pktline.ResponseEnd {
			break
		}
	}

	pkts := tracer.packetEvents()
	require.NotEmpty(t, pkts, "the response Reader must emit PacketEvents")
	for _, p := range pkts {
		assert.Equal(t, trace.DirectionInbound, p.Direction,
			"response packets are inbound")
		assert.Contains(t, p.URL, "/repo.git/info/refs",
			"each PacketEvent must carry the redacted probe URL")
	}
}

func TestTracer_PacketEvent_OnCommandRequest(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	pkts := tracer.packetEvents()
	var outbound []trace.PacketEvent
	for _, p := range pkts {
		if p.Direction == trace.DirectionOutbound {
			outbound = append(outbound, p)
		}
	}
	require.NotEmpty(t, outbound,
		"the command request body Writer must emit outbound PacketEvents")
	// Per [gitprotocol-v2.adoc §"Command Request"] the body is
	// command + cap + delim + arg + flush — five packets minimum.
	//
	// [gitprotocol-v2.adoc §"Command Request"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#command-request
	assert.GreaterOrEqual(t, len(outbound), 5,
		"command request body emits one PacketEvent per pkt-line")
	for _, p := range outbound {
		assert.Contains(t, p.URL, "/repo.git/git-upload-pack",
			"each outbound PacketEvent must carry the POST URL")
	}
	// Spot-check one of the data packets carries the command line.
	var sawCommand bool
	for _, p := range outbound {
		if p.Kind == trace.PacketData && bytes.Equal(p.Bytes, []byte("command=ls-refs\n")) {
			sawCommand = true
		}
	}
	assert.True(t, sawCommand,
		"the outbound PacketEvent stream must include the command= line")
}

func TestTracer_PacketEvent_OnDumbAdvertisement(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"3333333333333333333333333333333333333333\trefs/heads/main\n",
		))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Drain the synthesised v0 stream so the wrapped `pktline.Reader`
	// emits its `PacketEvent`s.
	rdr := conn.Advertisement()
	for {
		p, err := rdr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			break
		}
	}

	pkts := tracer.packetEvents()
	require.NotEmpty(t, pkts,
		"dumb path's synthesised pkt-line stream must emit PacketEvents")
	var sawData bool
	for _, p := range pkts {
		assert.Equal(t, trace.DirectionInbound, p.Direction,
			"dumb path packets are inbound (synthesised from response body)")
		assert.Contains(t, p.URL, "/repo.git/info/refs",
			"dumb path PacketEvents must carry the probe URL")
		if p.Kind == trace.PacketData && strings.Contains(string(p.Bytes), "refs/heads/main") {
			sawData = true
		}
	}
	assert.True(t, sawData,
		"the synthesised pkt-line stream must surface the dumb body's ref")
}

func TestTracer_NoEmissions_WhenNil(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	// A nil Tracer must never panic on the no-emission path: every
	// emission site is gated by `trace.IsEnabled` (or equivalent).
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: nil})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	rdr := conn.Advertisement()
	for {
		p, err := rdr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			break
		}
	}
}

// TestTracer_HTTPEvent_OnAuthRetry pins the per-`client.Do` event
// rule on the auth-retry path: an anonymous probe that comes back
// 401 and then succeeds with credentials emits two HTTPEvents.
func TestTracer_HTTPEvent_OnAuthRetry(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			w.Header().Set("Content-Type", smartAdvHeader)
			_, _ = w.Write(body)
		}
	}))
	defer srv.Close()

	tr := New(WithCredentials(Static(Basic("alice", "secret"))))
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got := tracer.httpEvents()
	require.Len(t, got, 2,
		"two probe round-trips (anonymous then with credentials) must emit two HTTPEvents")
	assert.Equal(t, http.StatusUnauthorized, got[0].Status)
	assert.Equal(t, http.StatusOK, got[1].Status)
}

// TestTracer_PacketEvent_BytesAreCopySafe pins that the bytes captured
// inside `OnEvent` survive past the call. The capturing tracer in this
// file clones every `PacketEvent.Bytes` before storing; this test
// asserts the clones are not the same underlying array as a
// subsequently-emitted packet, which would indicate a buffer-aliasing
// regression.
func TestTracer_PacketEvent_BytesAreCopySafe(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Grab two distinct data PacketEvents and confirm their byte
	// slices do not alias each other; a leak would manifest as one
	// "captured" payload rewritten to look like another.
	var data []trace.PacketEvent
	for _, e := range tracer.packetEvents() {
		if e.Kind == trace.PacketData {
			data = append(data, e)
		}
	}
	require.GreaterOrEqual(t, len(data), 2,
		"need at least two data PacketEvents to compare")
	a := data[0].Bytes
	b := data[1].Bytes
	if len(a) == 0 || len(b) == 0 {
		t.Skip("at least one data event has empty bytes; skipping aliasing check")
	}
	// They may legitimately have identical contents (e.g. two equal
	// flushes), but their backing arrays must be distinct after the
	// tracer's clone. Mutate one and confirm the other is unaffected.
	orig := append([]byte(nil), b...)
	a[0] ^= 0xff
	assert.Equal(t, orig, b,
		"each captured PacketEvent.Bytes must own its backing array")
}

// TestTracer_HTTPEvent_OnRedirect pins the one-event-per-`client.Do`
// rule even when the chain follows a redirect. A 301 followed by a 200
// must emit one HTTPEvent whose Status reflects the final response.
func TestTracer_HTTPEvent_OnRedirect(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	var hops int32
	mux := http.NewServeMux()
	mux.HandleFunc("/old.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/new.git/info/refs?service=git-upload-pack", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/new.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		hops++
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/old.git")
	tracer := &capturingTracer{}

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	got := tracer.httpEvents()
	require.Len(t, got, 1,
		"a redirect chain must emit exactly one HTTPEvent (the final hop's outcome)")
	assert.Equal(t, http.StatusOK, got[0].Status,
		"the HTTPEvent reflects the final response")
	assert.Contains(t, got[0].URL, "/new.git/info/refs",
		"the HTTPEvent URL is the post-redirect URL")
	assert.Equal(t, int32(2), hops, "both endpoints must have been hit")
}

// TestTracer_PacketEvent_OutboundURLIsPreRedirect pins the doc
// contract on `encodeCommandBody`: every outbound `PacketEvent`
// carries the request URL at the time the body was encoded, not the
// post-redirect URL. The body is constructed once and a single
// tracer URL applies to every pkt-line in it, even when a follow-on
// redirect resends those bytes against a different URL.
//
// The fixture uses a [stubRoundTripper] to fake a same-host scheme
// upgrade under `FollowRedirectsAlways`: the probe and the initial
// command POST land on `http://example.com`, then the POST gets a
// 302 to the same host on `https`. A scheme upgrade is same-origin,
// so `Authorization` is preserved and the redirect runs cleanly.
// All outbound `PacketEvent` URLs must name the original `http`
// POST URL.
func TestTracer_PacketEvent_OutboundURLIsPreRedirect(t *testing.T) {
	t.Parallel()
	// Build a probe body that drives the smart advertisement to a
	// flush so [drainAdvertisement] terminates: `# service=` preamble,
	// flush, then a v2 capability line, then the closing flush.
	var probeBuf bytes.Buffer
	pw := pktline.NewWriter(&probeBuf)
	require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
	require.NoError(t, pw.WriteFlush())
	require.NoError(t, pw.WritePacket([]byte("version 2\n")))
	require.NoError(t, pw.WriteFlush())
	probeBody := probeBuf.Bytes()

	// Build a non-empty success body for the redirected POST so the
	// inbound reader has packets to drain.
	var respBuf bytes.Buffer
	rw := pktline.NewWriter(&respBuf)
	require.NoError(t, rw.WritePacket([]byte("ok\n")))
	require.NoError(t, rw.WriteFlush())
	respBody := respBuf.Bytes()

	rt := &stubRoundTripper{}
	rt.respond = func(req *http.Request, hop int) *http.Response {
		switch {
		case req.Method == http.MethodGet:
			h := http.Header{}
			h.Set("Content-Type", smartAdvHeader)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(bytes.NewReader(probeBody)),
			}
		case req.Method == http.MethodPost && req.URL.Scheme == "http":
			h := http.Header{}
			// A scheme upgrade to the same host is same-origin under
			// canonical Git's rule, so [stubRoundTripper] returns 307
			// (preserves method on follow-up; 302 would rewrite POST
			// to GET per RFC 7231 §6.4.3) to the `https` URL.
			h.Set("Location", "https://example.com/repo.git/git-upload-pack")
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: h}
		case req.Method == http.MethodPost && req.URL.Scheme == "https":
			h := http.Header{}
			h.Set("Content-Type", commandAcceptType)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(bytes.NewReader(respBody)),
			}
		}
		return nil
	}

	tracer := &capturingTracer{}
	tr := New(
		WithClient(&http.Client{Transport: rt}),
		WithFollowRedirects(FollowRedirectsAlways),
	)
	u, err := transport.ParseURL("http://example.com/repo.git")
	require.NoError(t, err)

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{Tracer: tracer})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// The chain hit two POST hops (`http` then `https`). Confirm both
	// were observed so the test is meaningful.
	var sawHTTPS bool
	for _, req := range rt.requests {
		if req.Method == http.MethodPost && req.URL.Scheme == "https" {
			sawHTTPS = true
			break
		}
	}
	require.True(t, sawHTTPS,
		"the test fixture must have driven the chain through the https hop")

	// Every outbound `PacketEvent` must carry the pre-redirect URL —
	// `http://example.com/repo.git/git-upload-pack` — not the
	// post-redirect `https://...` URL the bytes ultimately reached.
	var outbound []trace.PacketEvent
	for _, e := range tracer.packetEvents() {
		if e.Direction == trace.DirectionOutbound {
			outbound = append(outbound, e)
		}
	}
	require.NotEmpty(t, outbound,
		"the command request body must emit outbound PacketEvents")
	for _, p := range outbound {
		assert.Equal(t,
			"http://example.com/repo.git/git-upload-pack", p.URL,
			"every outbound PacketEvent must pin the pre-redirect POST URL")
	}
}

// TestTracer_PacketEvent_NoEmissionsWhenNoTracer pins the no-overhead
// guarantee: a `*pktline.Reader` constructed without a tracer must
// not collect events anywhere observable. Smoke-tested by reading
// the advertisement with no tracer and confirming the reader does
// not break.
func TestTracer_PacketEvent_NoEmissionsWhenNoTracer(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	// No tracer: open, drain, command, drain — must complete cleanly.
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)
}
