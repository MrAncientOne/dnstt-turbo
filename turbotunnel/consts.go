// Package turbotunnel is facilities for embedding packet-based reliability
// protocols inside other protocols.
//
// https://github.com/net4people/bbs/issues/9
package turbotunnel

import (
	"errors"
	"sync"
)

// queueSize is the size of send and receive queues in QueuePacketConn and
// RemoteMap. The client/server grows it at startup to comfortably hold the
// configured KCP window (see -kcp-window): packets beyond the queue are
// dropped, so the queue must be >= the window. Default 256 (128-segment
// window) matches the tuned baseline.
//
// It lives behind a RWMutex (not a bare var) because queue creation reads
// it on the packet path — including background expiry sweepers — while
// SetQueueSize writes it: a bare var is a data race the detector fires on
// (and a torn read would size a channel wrong). Set once at startup via
// SetQueueSize before any QueuePacketConn is created; reads take the read
// lock, which is uncontended after startup.
var (
	queueSizeMu sync.RWMutex
	queueSize   = 256
)

// GetQueueSize reports the current queue size.
func GetQueueSize() int {
	queueSizeMu.RLock()
	defer queueSizeMu.RUnlock()
	return queueSize
}

// SetQueueSize sets the queue size to n (non-positive values ignored).
// Call once at startup, before creating any QueuePacketConn.
func SetQueueSize(n int) {
	if n <= 0 {
		return
	}
	queueSizeMu.Lock()
	defer queueSizeMu.Unlock()
	queueSize = n
}

var errClosedPacketConn = errors.New("operation on closed connection")
var errNotImplemented = errors.New("not implemented")

// DummyAddr is a placeholder net.Addr, for when a programming interface
// requires a net.Addr but there is none relevant. All DummyAddrs compare equal
// to each other.
type DummyAddr struct{}

func (addr DummyAddr) Network() string { return "dummy" }
func (addr DummyAddr) String() string  { return "dummy" }
