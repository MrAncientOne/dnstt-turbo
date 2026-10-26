package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
)

func TestFormatRatesFields(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	out := formatRates(ts, 1540000, 1200000, 32*time.Millisecond, bbr.StateProbeBW, false, 1800000)
	for _, want := range []string{
		"VERSION=1\n", "TS=1700000000\n", "MAXBW_BPS=1540000\n",
		"RTT_MS=32\n", "STATE=probebw\n", "PROFILE=calm\n",
		"HEALTH=855\n", // 1000*1540000/1800000 = 855.5 -> 855
		"SUST_BPS=1200000\n",
		"BEST_BPS=1800000\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatRatesDegradedAndBounds(t *testing.T) {
	out := formatRates(time.Unix(1, 0), 3000, 2500, 0, bbr.StateStartup, true, 1500000)
	if !strings.Contains(out, "PROFILE=degraded\n") {
		t.Fatalf("want degraded profile in:\n%s", out)
	}
	if !strings.Contains(out, "HEALTH=2\n") {
		t.Fatalf("want HEALTH=2 in:\n%s", out)
	}
	// No anchor yet (best 0): assume healthy, never divide by zero.
	out = formatRates(time.Unix(1, 0), 250000, 200000, 100*time.Millisecond, bbr.StateStartup, false, 0)
	if !strings.Contains(out, "HEALTH=1000\n") {
		t.Fatalf("want HEALTH=1000 with no best in:\n%s", out)
	}
	// Over-peak clamps at 1000, never above.
	out = formatRates(time.Unix(1, 0), 2000000, 1900000, 100*time.Millisecond, bbr.StateProbeBW, false, 1500000)
	if !strings.Contains(out, "HEALTH=1000\n") {
		t.Fatalf("want clamped HEALTH=1000 in:\n%s", out)
	}
}

func TestPublishRatesAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "rates")
	if err := publishRates(path, "VERSION=1\n"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "VERSION=1\n" {
		t.Fatalf("read back %q, err %v", b, err)
	}
	// Rewrite replaces atomically; no tmp residue remains.
	if err := publishRates(path, "VERSION=1\nTS=2\n"); err != nil {
		t.Fatalf("republish: %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(dir, "sub"))
	if len(ents) != 1 || ents[0].Name() != "rates" {
		t.Fatalf("tmp residue: %v", ents)
	}
}
