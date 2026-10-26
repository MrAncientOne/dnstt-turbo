package dns

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestWriteNameRejectsInvalid: programmatically built Names with empty or
// overlong labels must return errors, never panic (the encoder handles
// wire-derived names on the server; a crash there kills all sessions).
func TestWriteNameRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name Name
		want error
	}{
		{Name{[]byte("")}, ErrZeroLengthLabel},
		{Name{[]byte("ok"), []byte("")}, ErrZeroLengthLabel},
		{Name{bytes.Repeat([]byte{'a'}, 64)}, ErrLabelTooLong},
		{Name{[]byte("ok"), bytes.Repeat([]byte{'b'}, 100)}, ErrLabelTooLong},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("WriteName(%q) panicked: %v", tc.name, r)
				}
			}()
			b := newMessageBuilder()
			err := b.WriteName(tc.name)
			if !errors.Is(err, tc.want) {
				t.Fatalf("WriteName(%q) = %v, want %v", tc.name, err, tc.want)
			}
		}()
	}
	// Boundary labels 1..63 encode cleanly.
	for n := 1; n <= 63; n++ {
		b := newMessageBuilder()
		if err := b.WriteName(Name{bytes.Repeat([]byte{'x'}, n)}); err != nil {
			t.Fatalf("WriteName(%d-byte label) = %v, want nil", n, err)
		}
	}
}

// TestWriteQuestionPropagatesNameError: an invalid question name fails the
// whole message encode with the label error, not a panic and not a
// truncated message.
func TestWriteQuestionPropagatesNameError(t *testing.T) {
	msg := &Message{
		ID:    1,
		Flags: 0x0100,
		Question: []Question{
			{Name: Name{bytes.Repeat([]byte{'q'}, 64)}, Type: RRTypeTXT, Class: ClassIN},
		},
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("WireFormat panicked: %v", r)
			}
		}()
		if _, err := msg.WireFormat(); !errors.Is(err, ErrLabelTooLong) {
			t.Fatalf("WireFormat = %v, want ErrLabelTooLong", err)
		}
	}()
}

// TestNameSizeBoundaries: 63/64-byte labels and 255/256-byte names on both
// the build and parse paths.
func TestNameSizeBoundaries(t *testing.T) {
	label63 := bytes.Repeat([]byte{'a'}, 63)
	if _, err := NewName([][]byte{label63}); err != nil {
		t.Fatalf("63-byte label rejected: %v", err)
	}
	if _, err := NewName([][]byte{append(label63, 'x')}); !errors.Is(err, ErrLabelTooLong) {
		t.Fatalf("64-byte label err = %v, want ErrLabelTooLong", err)
	}
	// Four labels totalling a 255-octet encoding is the max.
	maxLabels := [][]byte{
		bytes.Repeat([]byte{'a'}, 63),
		bytes.Repeat([]byte{'b'}, 63),
		bytes.Repeat([]byte{'c'}, 63),
		bytes.Repeat([]byte{'d'}, 61),
	}
	maxName, err := NewName(maxLabels)
	if err != nil {
		t.Fatalf("max name rejected: %v", err)
	}
	wire, err := (&Message{ID: 7, Flags: 0x0100,
		Question: []Question{{Name: maxName, Type: RRTypeTXT, Class: ClassIN}},
	}).WireFormat()
	if err != nil {
		t.Fatalf("max name encode: %v", err)
	}
	back, err := MessageFromWireFormat(wire)
	if err != nil {
		t.Fatalf("max name re-parse: %v", err)
	}
	if back.Question[0].Name.String() != maxName.String() {
		t.Fatalf("round trip changed the name")
	}
	// One byte more must fail the build.
	maxLabels[3] = append(maxLabels[3], 'e', 'x')
	if _, err := NewName(maxLabels); !errors.Is(err, ErrNameTooLong) {
		t.Fatalf("256-octet name err = %v, want ErrNameTooLong", err)
	}
}

// TestHugeSectionCountFailsFast: a header claiming 65535 questions must not
// hang or over-allocate; it must error.
func TestHugeSectionCountFailsFast(t *testing.T) {
	var hdr bytes.Buffer
	hdr.Write([]byte{0x12, 0x34, 0x01, 0x00, 0xff, 0xff, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	done := make(chan error, 1)
	go func() {
		_, err := MessageFromWireFormat(hdr.Bytes())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("65535-question header parsed without error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("huge section count hung the parser")
	}
}

// TestPointerChainBoundary: 10 compression-pointer follows parse; the 11th
// fails with ErrTooManyPointers. (readName allows numPointers <=
// compressionPointerLimit=10.) The chain hides inside an answer's
// length-delimited RDATA so the message has no trailing bytes: the answer
// name is one pointer plus `cells` chained cells ending in a root label,
// for 1+cells total follows.
func TestPointerChainBoundary(t *testing.T) {
	build := func(cells int) []byte {
		var b bytes.Buffer
		b.Write([]byte{0xab, 0xcd, 0x81, 0x80, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})
		b.Write([]byte{0x01, 'a', 0x00, 0x00, 0x10, 0x00, 0x01}) // question "a" TXT IN
		chainAt := 19 + 2 + 2 + 2 + 4 + 2                        // answer NAME(2)+TYPE+CLASS+TTL+RDLEN
		b.Write([]byte{0xc0, byte(chainAt)})                     // answer NAME -> chain
		b.Write([]byte{0x00, 0x10, 0x00, 0x01})                  // TYPE TXT CLASS IN
		b.Write([]byte{0x00, 0x00, 0x00, 0x3c})                  // TTL
		b.Write([]byte{0x00, byte(2*cells + 1)})                 // RDLEN
		for i := 0; i < cells; i++ {
			next := chainAt + 2*(i+1)
			b.Write([]byte{0xc0, byte(next)})
		}
		b.WriteByte(0x00) // root terminator
		return b.Bytes()
	}
	// 1 + 9 = 10 follows: at the limit, parses.
	if _, err := MessageFromWireFormat(build(9)); err != nil {
		t.Fatalf("10-follow chain rejected: %v", err)
	}
	// 1 + 10 = 11 follows: over the limit.
	if _, err := MessageFromWireFormat(build(10)); !errors.Is(err, ErrTooManyPointers) {
		t.Fatalf("11-follow chain err = %v, want ErrTooManyPointers", err)
	}
	// A 2-cell self-loop (pointer cycle) also terminates, via the limit.
	cycle := build(0)
	// Patch: answer NAME -> its own offset (19), RDATA[0..1] -> 19.
	cycle[19] = 0xc0
	cycle[20] = 19
	if _, err := MessageFromWireFormat(cycle); !errors.Is(err, ErrTooManyPointers) {
		t.Fatalf("pointer cycle err = %v, want ErrTooManyPointers", err)
	}
}

// TestWireFormatNeverPanicsOnParsedInput: any message that parses must
// re-encode without error or panic (the property fuzz.go asserts under
// gofuzz, pinned here as a unit test over nasty seeds).
func TestWireFormatNeverPanicsOnParsedInput(t *testing.T) {
	seeds := [][]byte{
		{},
		{0x00},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		bytes.Repeat([]byte{0xc0}, 100),
		bytes.Repeat([]byte{0x3f}, 300),
	}
	for i, s := range seeds {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("seed %d panicked: %v", i, r)
				}
			}()
			msg, err := MessageFromWireFormat(s)
			if err != nil {
				return
			}
			wire, err := msg.WireFormat()
			if err != nil {
				t.Fatalf("seed %d: parsed but re-encode failed: %v", i, err)
			}
			back, err := MessageFromWireFormat(wire)
			if err != nil {
				t.Fatalf("seed %d: re-parse failed: %v", i, err)
			}
			if len(back.Question) != len(msg.Question) || len(back.Answer) != len(msg.Answer) {
				t.Fatalf("seed %d: section counts changed across round trip", i)
			}
		}()
	}
}

// TestDecodeRDataTXTEdges: empty input, dangling length, exact-fit.
func TestDecodeRDataTXTEdges(t *testing.T) {
	if _, err := DecodeRDataTXT(nil); err == nil {
		t.Fatal("empty TXT decoded without error")
	}
	if _, err := DecodeRDataTXT([]byte{0x05, 'a', 'b'}); err == nil {
		t.Fatal("dangling TXT length decoded without error")
	}
	p, err := DecodeRDataTXT([]byte{0x03, 'a', 'b', 'c'})
	if err != nil || string(p) != "abc" {
		t.Fatalf("exact-fit TXT = %q, %v", p, err)
	}
	// 255-byte character-string boundary round trip.
	big := bytes.Repeat([]byte{'z'}, 600)
	enc := EncodeRDataTXT(big)
	dec, err := DecodeRDataTXT(enc)
	if err != nil || !bytes.Equal(dec, big) {
		t.Fatalf("600-byte TXT round trip failed: %v", err)
	}
}

// TestErrorStringsStable: sentinel errors keep their text (logs and tests
// match on them).
func TestErrorStringsStable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{ErrZeroLengthLabel, "zero-length"},
		{ErrLabelTooLong, "63"},
		{ErrNameTooLong, "255"},
		{ErrTooManyPointers, "compression"},
		{ErrIntegerOverflow, "overflow"},
	} {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("error %q missing %q", tc.err, tc.want)
		}
	}
}
