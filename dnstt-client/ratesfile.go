package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
)

// defaultRatesPath is where the BBR ticker publishes its live estimate for
// out-of-process consumers (cake-autorate.sh). Override with BBR_RATES_FILE.
// A plain KEY=VALUE file: trivially parseable from shell, no new deps.
const defaultRatesPath = "/var/run/dnstt-bbr-rates"

// formatRates renders one Snapshot of estimator state. All rates are whole
// bytes/second; HEALTH is permille of session-best (1000 = at peak).
// SUST is the sustained (windowed-median) companion to the windowed-max
// MAXBW: shapers target SUST, pacing and window target MAXBW.
// Pure function: unit-tested, no I/O.
func formatRates(ts time.Time, maxBW, sustBW float64, rtt time.Duration, state bbr.State, degraded bool, best float64) string {
	// The controller only produces finite non-negative rates, but the
	// shell consumer parses these with arithmetic: never emit NaN,
	// negative, or infinite fields (float-to-int of NaN is garbage).
	maxBW = saneRate(maxBW)
	sustBW = saneRate(sustBW)
	best = saneRate(best)
	if rtt < 0 {
		rtt = 0
	}
	health := 1000
	if best > 0 {
		health = int(1000 * maxBW / best)
		if health < 0 {
			health = 0
		}
		if health > 1000 {
			health = 1000
		}
	}
	prof := "calm"
	if degraded {
		prof = "degraded"
	}
	return fmt.Sprintf("VERSION=1\nTS=%d\nMAXBW_BPS=%d\nSUST_BPS=%d\nRTT_MS=%d\nSTATE=%s\nPROFILE=%s\nHEALTH=%d\nBEST_BPS=%d\n",
		ts.Unix(), int64(maxBW), int64(sustBW), rtt.Milliseconds(), state.String(), prof, health, int64(best))
}

// saneRate maps a rate to a finite non-negative value for rendering:
// NaN, negative, and +Inf inputs (all impossible from the controller,
// all possible from a future caller bug) become 0 instead of garbage
// or overflowed integer fields.
func saneRate(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// publishRates atomically replaces path with content (tmp file + rename,
// so readers never see a torn write). It creates the parent dir on demand.
// Any error returns to the caller; the ticker treats failure as silent
// degradation because the consumer (cake-autorate.sh) already falls back
// to static rates on a stale or missing file.
func publishRates(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bbr-rates-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Sync the content before the rename so a crash or power loss can
	// leave a stale file but never a torn one; the consumer already
	// treats stale as "fall back to static". (Directory fsync after
	// rename is omitted deliberately: the fallback covers a lost
	// rename, and fsyncing /var/run's parent on every 1s publish would
	// serialize on the filesystem journal for no live benefit.)
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
