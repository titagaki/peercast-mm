package servent

import "sync"

// outputBase is the common notification and lifecycle state shared by
// PCPOutputStream and HTTPOutputStream.
type outputBase struct {
	conn       *countingConn
	id         int
	remoteAddr string

	mu       sync.Mutex
	closed   bool
	headerCh chan struct{}
	infoCh   chan struct{}
	trackCh  chan struct{}
	closeCh  chan struct{}
	onClose  func() // optional hook called within the close mutex
}

func newOutputBase(conn *countingConn, id int) outputBase {
	return outputBase{
		conn:       conn,
		id:         id,
		remoteAddr: conn.RemoteAddr().String(),
		headerCh:   make(chan struct{}, 1),
		infoCh:     make(chan struct{}, 1),
		trackCh:    make(chan struct{}, 1),
		closeCh:    make(chan struct{}),
	}
}

// NotifyHeader signals a header change.
func (b *outputBase) NotifyHeader() { notify(b.headerCh) }

// NotifyInfo signals an info change.
func (b *outputBase) NotifyInfo() { notify(b.infoCh) }

// NotifyTrack signals a track change.
func (b *outputBase) NotifyTrack() { notify(b.trackCh) }

// Close terminates the output stream. Safe to call multiple times.
func (b *outputBase) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.closeCh)
		if b.onClose != nil {
			b.onClose()
		}
	}
}

// ID returns the unique connection identifier.
func (b *outputBase) ID() int { return b.id }

// RemoteAddr returns the remote address string.
func (b *outputBase) RemoteAddr() string { return b.remoteAddr }

// SendRate returns bytes sent per second (last full second).
func (b *outputBase) SendRate() int64 { return b.conn.sent.rate() }
