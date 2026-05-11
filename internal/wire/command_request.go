package wire

import (
	"github.com/hiddeco/go-ls-remote/pktline"
)

// EncodeV2CommandRequest writes a v2 command-request frame to w using
// the canonical layout from `gitprotocol-v2.adoc` §"Command Request"
// and matched by `serve.c::process_request`:
//
//	command-request = command-line *capability-line delim-pkt *arg-line flush-pkt
//	command-line    = PKT-LINE("command=" cmd LF)
//	capability-line = PKT-LINE(cap LF)
//	arg-line        = PKT-LINE(arg LF)
//
// Trailing LF on each line matches `packet_write_fmt(... "\n")` in
// canonical Git's writer. A write failure short-circuits the rest of
// the frame: the underlying [pktline.Writer] writes each pkt-line in a
// single [io.Writer.Write] call, so a partial frame on the wire is
// possible only when the underlying sink itself fails mid-frame, in
// which case there is nothing more this side can usefully emit. The
// wire is then in a corrupted mid-frame state the server cannot
// recover from, so the connection must be considered dead and the
// caller must tear it down.
//
// EncodeV2CommandRequest does not flush the underlying writer beyond
// the closing pkt-line flush — wrapping or `bytes.Buffer` finalisation
// is left to the caller. When the sink is a `bytes.Buffer` write
// itself cannot fail, but the encoder may still return a wrapped
// [pktline.ErrPayloadTooLarge] if any value (command name, capability,
// or argument) exceeds the canonical cap from `pkt-line.h:234` once
// the trailing LF is appended.
func EncodeV2CommandRequest(w *pktline.Writer, name string, args, caps []string) error {
	if err := writeLine(w, "command="+name); err != nil {
		return err
	}
	for _, c := range caps {
		if err := writeLine(w, c); err != nil {
			return err
		}
	}
	if err := w.WriteDelim(); err != nil {
		return err
	}
	for _, a := range args {
		if err := writeLine(w, a); err != nil {
			return err
		}
	}
	return w.WriteFlush()
}
