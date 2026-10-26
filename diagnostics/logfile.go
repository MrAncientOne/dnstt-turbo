package diagnostics

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// RegisterLogFlags keeps stderr/journald output and optionally adds a bounded,
// append-only log. No arguments, keys or payloads are dumped automatically.
func RegisterLogFlags() func() error {
	path := flag.String("log-file", "", "also append diagnostics to this file (stderr always retained)")
	max := flag.Int64("log-max-bytes", 10*1024*1024, "rotate log-file at this size; keep three backups")
	return func() error {
		if *max < 1024 {
			return fmt.Errorf("log-max-bytes must be at least 1024")
		}
		if *path == "" {
			return nil
		}
		w, err := openRotating(*path, *max)
		if err != nil {
			return fmt.Errorf("opening log-file: %w", err)
		}
		log.SetOutput(io.MultiWriter(os.Stderr, w))
		return nil
	}
}

type rotatingWriter struct {
	mu        sync.Mutex
	path      string
	max, size int64
	f         *os.File
}

func openRotating(path string, max int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, max: max}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		if err := w.f.Close(); err != nil {
			w.f = nil
			return 0, err
		}
		w.f = nil
		// Rename replaces the oldest backup. Errors retain stderr output;
		// a later call retries opening the live log rather than losing it.
		for i := 2; i >= 0; i-- {
			from := w.path
			if i > 0 {
				from = fmt.Sprintf("%s.%d", w.path, i)
			}
			if err := os.Rename(from, fmt.Sprintf("%s.%d", w.path, i+1)); err != nil && !os.IsNotExist(err) {
				return 0, err
			}
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}
