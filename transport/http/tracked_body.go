package httpt

import (
	"io"
	"sync"
)

// trackedBody wraps an HTTP response body so the [Conn] can observe
// the moment the caller finishes with it. `cleanup` runs exactly
// once — at the first terminal `Read` outcome (`io.EOF` or any
// other error) or on the first `Close`. The [Conn] uses this to
// deregister the body from its in-flight set as soon as the caller
// drains its reader, so a long-lived [Conn] issuing many commands
// does not accumulate one map entry per command for its lifetime.
//
// `Close` always closes the inner body, even when `cleanup` already
// ran via a `Read`-side trigger. `Read` errors propagate verbatim;
// the cleanup is a side effect that does not alter the bytes the
// caller sees.
type trackedBody struct {
	inner   io.ReadCloser
	cleanup func()
	once    sync.Once
}

// doCleanup invokes the cleanup callback if one was set. Called
// inside `once.Do` so it runs at most once per `trackedBody`.
func (t *trackedBody) doCleanup() {
	if t.cleanup != nil {
		t.cleanup()
	}
}

// Read delegates to the inner body. On any non-nil error (including
// `io.EOF`) the cleanup callback runs exactly once.
func (t *trackedBody) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if err != nil {
		t.once.Do(t.doCleanup)
	}
	return n, err
}

// Close runs cleanup (if it has not already run) and closes the
// inner body. Calling Close more than once is safe: cleanup runs at
// most once, and the second-and-later inner Close is the underlying
// type's contract.
func (t *trackedBody) Close() error {
	t.once.Do(t.doCleanup)
	return t.inner.Close()
}
