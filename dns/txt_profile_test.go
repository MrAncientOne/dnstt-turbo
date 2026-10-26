package dns

import (
	"bytes"
	"fmt"
	"testing"
)

func TestTXTCodecAllocationBudget(t *testing.T) {
	payload := bytes.Repeat([]byte{0x7b}, 16027)
	wire := EncodeRDataTXT(payload)
	for _, encode := range []bool{true, false} {
		t.Run(fmt.Sprint(encode), func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				if encode {
					_ = EncodeRDataTXT(payload)
				} else {
					p, err := DecodeRDataTXT(wire)
					if err != nil || !bytes.Equal(p, payload) {
						t.Fatal("codec integrity failure")
					}
				}
			})
			if allocs > 1 {
				t.Fatalf("TXT codec allocated %.0f objects for one response", allocs)
			}
		})
	}
}

func TestTXTCodecOwnsOutput(t *testing.T) {
	for _, size := range []int{0, 1, 254, 255, 256, 510, 511, 16027, 65535} {
		payload := bytes.Repeat([]byte{0x61}, size)
		wire := EncodeRDataTXT(payload)
		original := append([]byte(nil), wire...)
		clear(payload)
		if !bytes.Equal(wire, original) {
			t.Fatalf("encoding aliases input at %d bytes", size)
		}
		decoded, err := DecodeRDataTXT(wire)
		if err != nil || !bytes.Equal(decoded, bytes.Repeat([]byte{0x61}, size)) {
			t.Fatalf("roundtrip failed at %d bytes: %v", size, err)
		}
		clear(wire)
		if !bytes.Equal(decoded, bytes.Repeat([]byte{0x61}, size)) {
			t.Fatalf("decoding aliases input at %d bytes", size)
		}
	}
}

func BenchmarkTXTCodec(b *testing.B) {
	payload := bytes.Repeat([]byte{0x61}, 16027)
	wire := EncodeRDataTXT(payload)
	for _, encode := range []bool{true, false} {
		b.Run(fmt.Sprint(encode), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if encode {
					_ = EncodeRDataTXT(payload)
				} else {
					_, err := DecodeRDataTXT(wire)
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// Keep the previous streaming encoder as a differential compatibility oracle.
func FuzzTXTWireCompatibility(f *testing.F) {
	for _, size := range []int{0, 1, 255, 256, 510, 16027, 65535} {
		f.Add(bytes.Repeat([]byte{byte(size)}, size))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65535 {
			return
		}
		var legacy bytes.Buffer
		remaining := data
		for len(remaining) > 255 {
			legacy.WriteByte(255)
			legacy.Write(remaining[:255])
			remaining = remaining[255:]
		}
		legacy.WriteByte(byte(len(remaining)))
		legacy.Write(remaining)
		wire := EncodeRDataTXT(data)
		if !bytes.Equal(wire, legacy.Bytes()) {
			t.Fatal("wire encoding changed")
		}
		decoded, err := DecodeRDataTXT(wire)
		if err != nil || !bytes.Equal(decoded, data) {
			t.Fatalf("roundtrip changed: %v", err)
		}
	})
}
