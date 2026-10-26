package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaneRate: the renderer never emits NaN/negative/infinite fields.
func TestSaneRate(t *testing.T) {
	for _, v := range []float64{1, 0, 1e12} {
		if got := saneRate(v); got != v {
			t.Fatalf("saneRate(%v) = %v, want identity", v, got)
		}
	}
	for _, v := range []float64{-1, -1e18} {
		if got := saneRate(v); got != 0 {
			t.Fatalf("saneRate(%v) = %v, want 0", v, got)
		}
	}
	for _, v := range []float64{nanRate(), infRate(), ninfRate()} {
		if got := saneRate(v); got != 0 {
			t.Fatalf("saneRate(%v) = %v, want 0", v, got)
		}
	}
}

func nanRate() float64  { return 0.0 / zeroRate() }
func zeroRate() float64 { return 0.0 }
func infRate() float64  { return 1.0 / zeroRate() }
func ninfRate() float64 { return -1.0 / zeroRate() }

// TestPublishRatesUnwritableCleansTmp: a publish failure leaves no tmp
// residue behind for the shell consumer to trip over.
func TestPublishRatesUnwritableCleansTmp(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is a regular file: MkdirAll fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publishRates(filepath.Join(blocker, "rates"), "x"); err == nil {
		t.Fatal("publish through a file-parent succeeded, want error")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".bbr-rates-") {
			t.Fatalf("tmp residue %q left after failed publish", e.Name())
		}
	}
}

// TestPublishRatesConcurrent: concurrent publishers never corrupt the file;
// the content is always one whole publish (last-rename-wins).
func TestPublishRatesConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rates")
	done := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			done <- publishRates(path, "VERSION=1\nTS="+string(rune('0'+i))+"\n")
		}(i)
	}
	for i := 0; i < 16; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "VERSION=1\nTS=") || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("torn content %q", b)
	}
	if strings.Count(string(b), "VERSION=") != 1 {
		t.Fatalf("interleaved content %q", b)
	}
}
