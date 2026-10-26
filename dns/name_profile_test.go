package dns

import "testing"

func TestNewNameValidationAllocationBudget(t *testing.T) {
	labels := [][]byte{[]byte("randomquerypayload"), []byte("t"), []byte("example")}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := NewName(labels); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("validating an existing name allocated %.0f objects", allocs)
	}
}

func BenchmarkNameValidation(b *testing.B) {
	labels := [][]byte{[]byte("randomquerypayload"), []byte("t"), []byte("example")}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewName(labels); err != nil {
			b.Fatal(err)
		}
	}
}
