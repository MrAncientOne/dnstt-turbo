package dns

import (
	"testing"
)

// FuzzMessageFromWireFormat is the stdlib (Go 1.18+) coverage-guided fuzz
// target for the DNS parser: untrusted wire bytes, so the input space is
// exactly what fuzzing beats tables at. Invariants beyond no-panic:
// anything that parses must re-encode, and the re-encoding must re-parse
// with identical section counts (encode/decode round-trip stability).
//
// Tiers (see repo test policy): seed corpus runs under plain `go test`;
// post-merge `-fuzztime=60s`; nightly `-fuzztime=4h`. Failing inputs land
// in testdata/fuzz/ and become permanent regressions.
func FuzzMessageFromWireFormat(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x00},
		{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x03, 'f', 'o', 'o', 0x00, 0x00, 0x10, 0x00, 0x01}, // valid query
		{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0xc0, 0x0c, 0xc0, 0x0c, 0xc0, 0x0c, 0xc0, 0x0c},                         // pointer-heavy
		{0x00, 0x00, 0x01, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, // 64K questions claim
		{0x00, 0x00, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00,
			0x01, 'a', 0x00, 0x00, 0x10, 0x00, 0x01,
			0x01, 'a', 0x00, 0x00, 0x10, 0x00, 0x01,
			0x00, 0x00, 0x00, 0x3c, 0x00, 0x05, 0x03, 'x', 'y', 'z'}, // response w/ TXT
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := MessageFromWireFormat(data)
		if err != nil {
			return
		}
		wire, err := msg.WireFormat()
		if err != nil {
			t.Fatalf("parsed %d bytes but re-encode failed: %v", len(data), err)
		}
		back, err := MessageFromWireFormat(wire)
		if err != nil {
			t.Fatalf("re-parse of re-encoded message failed: %v", err)
		}
		if len(back.Question) != len(msg.Question) ||
			len(back.Answer) != len(msg.Answer) ||
			len(back.Authority) != len(msg.Authority) ||
			len(back.Additional) != len(msg.Additional) {
			t.Fatalf("section counts changed across round trip")
		}
	})
}
