# Local smux patch

Upstream: `github.com/xtaci/smux v1.5.24`, under the included MIT license.
All upstream Go files are retained, including tests. `session.go` is the only
modified upstream runtime file.

`OpenStream` registers the new stream before publishing SYN. Previously a fast
peer could send PSH, UPD or FIN before the local SYN write returned, while the
receive loop had no stream entry and silently discarded those frames. Failed
opens remove their provisional registration and recycle buffered tokens.
Wire version, stream IDs, framing and scheduling remain unchanged.

`fairqueue/TestSmuxRegistersStreamBeforeSendingSYN` forces this ordering over
real smux/net.Pipe; it failed three times on the unmodified dependency. Do not
remove the replacement merely to update a version: v1.5.57 also still registers
after the SYN write. Recheck the regression when updating upstream.

## Test maintenance

`session_test.go` also adjusts `TestGetDieCh` to use real sessions over
`net.Pipe`, close both sessions during cleanup, and make assertions on the
test goroutine. It checks that local `Close` closes that stream's `GetDieCh`
channel synchronously, while a peer FIN produces read EOF without closing the
receiver's local close channel. The peer EOF read has a one-second deadline.
Upstream's unjoined helper called `t.Fatal` from a non-test goroutine; an
earlier cleanup made that assertion effective but incorrectly waited for the
peer's local close channel. This change affects tests only; it leaves the
runtime implementation and protocol unchanged.
