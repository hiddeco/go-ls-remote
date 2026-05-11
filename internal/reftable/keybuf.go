package reftable

// keyBuf threads two scratch buffers across a block walk so each
// record decode reuses memory the previous decode no longer needs.
// The walker calls Next to obtain (prev, scratch), passes them to
// [decodeRefRecord], and calls Swap with the decoded key once the
// decoded record is safely committed (i.e., after the iterator yield
// returns).
//
// Correctness depends on a load-bearing invariant of [decodeKey]: it
// must copy prev[:prefixLen] into the destination buffer BEFORE
// overwriting any bytes of scratch. The two buffers must therefore
// never alias each other; the ping-pong design enforces this by
// keeping the role of "prev" and "scratch" tied to distinct
// underlying arrays.
//
// On the first call both buffers are nil and the first decode
// allocates fresh; on the second decode prev holds the first key but
// scratch is still nil, so the second decode also allocates. From the
// third decode onward both buffers are sized for the namespace and
// reuse is the steady state.
type keyBuf struct {
	prev, scratch []byte
}

// Next returns the previous decoded key and a buffer to decode the
// next key into. The two are guaranteed not to share an underlying
// array.
func (b *keyBuf) Next() (prev, scratch []byte) {
	return b.prev, b.scratch
}

// Swap promotes key to "previous" and rotates the formerly-previous
// buffer to "scratch" so the next decode may reuse it. Call after the
// decoded record has been safely committed downstream (e.g. after the
// iterator's yield returns, or after the in-block compare resolves).
func (b *keyBuf) Swap(key []byte) {
	b.prev, b.scratch = key, b.prev
}
