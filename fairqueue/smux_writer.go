package fairqueue

import (
	"time"

	"github.com/xtaci/smux"
)

// smuxWriter owns one outstanding write across scheduler turns. A smux write
// deadline can return before an enqueued frame is transmitted, so retrying that
// Write would duplicate data. Instead, only our wait expires: subsequent turns
// await the same result. The stream owner must close the stream/session when
// abandoning a flow, which also releases the outstanding write.
// Methods are called only by the scheduler's single writer.
type smuxWriter struct {
	stream   *smux.Stream
	deadline time.Time
	pending  chan smuxWriteResult
}

type smuxWriteResult struct {
	n   int
	err error
}

func (w *smuxWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = t
	return nil
}

func (w *smuxWriter) Write(p []byte) (int, error) {
	if w.pending == nil {
		// Own the bytes even if the caller abandons this flow and reuses p.
		data := append([]byte(nil), p...)
		w.pending = make(chan smuxWriteResult, 1)
		result := w.pending
		go func() {
			n, err := w.stream.Write(data)
			result <- smuxWriteResult{n, err}
		}()
	}
	var timeout <-chan time.Time
	if !w.deadline.IsZero() {
		timer := time.NewTimer(time.Until(w.deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case result := <-w.pending:
		w.pending = nil
		return result.n, result.err
	case <-timeout:
		return 0, smux.ErrTimeout
	}
}
