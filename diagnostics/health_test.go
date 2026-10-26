package diagnostics

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFailureStormRetainsCountsAndBoundsOutput(t *testing.T) {
	var out bytes.Buffer
	old := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(old)
	h := New("test")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Failure("dial_failed", errors.New("refused\nforged log line"))
				h.Received(7)
			}
		}()
	}
	wg.Wait()
	stop := h.Start()
	stop()
	stop()
	if h.Failures.Load() != 2000 || h.RxBytes.Load() != 14000 {
		t.Fatal("lost counters")
	}
	if strings.Count(out.String(), "event=dial_failed ") != 1 {
		t.Fatal("failure logging not throttled")
	}
	if !strings.Contains(out.String(), "kind=dial_failed count=2000") {
		t.Fatal("summary lost suppressed failures")
	}
	if strings.Count(out.String(), "event=health ") != 1 {
		t.Fatal("stop not idempotent")
	}
	if strings.Contains(out.String(), "\nforged log line") {
		t.Fatal("error injected unescaped log line")
	}
}

func TestAppendAndRotationRetainWholeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.log")
	if err := os.WriteFile(path, []byte("previous-run\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := openRotating(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if w.f != nil {
			w.f.Close()
		}
	}()
	w.Write([]byte("new-run\n"))
	data, _ := os.ReadFile(path)
	if string(data) != "previous-run\nnew-run\n" {
		t.Fatal("startup truncated history")
	}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := w.Write([]byte(fmt.Sprintf("record-%04d\n", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	files, _ := filepath.Glob(path + "*")
	if len(files) != 4 {
		t.Fatalf("expected live log + 3 backups, got %d", len(files))
	}
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 128 {
			t.Fatalf("unbounded file: %d", len(b))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if len(line) != 11 || !strings.HasPrefix(line, "record-") || seen[line] {
				t.Fatalf("torn or repeated record %q", line)
			}
			seen[line] = true
		}
	}
}
