package servent

import (
	"net"
	"sync"
	"time"
)

// byteCounter tracks bytes transferred per second using a two-bucket approach.
// The previous full-second bucket is returned as the current rate.
type byteCounter struct {
	mu       sync.Mutex
	current  int64
	previous int64
	bucketT  time.Time
}

func (c *byteCounter) add(n int) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.bucketT.IsZero() {
		c.bucketT = now
	}
	if now.Sub(c.bucketT) >= time.Second {
		c.previous = c.current
		c.current = 0
		c.bucketT = now
	}
	c.current += int64(n)
}

// rate returns the number of bytes transferred in the last complete second.
// Returns 0 if no data has been transferred recently.
func (c *byteCounter) rate() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bucketT.IsZero() {
		return 0
	}
	elapsed := time.Since(c.bucketT)
	if elapsed >= 2*time.Second {
		// No writes for 2+ seconds; stale data.
		c.previous = 0
		c.current = 0
		return 0
	}
	if elapsed >= time.Second {
		// Rotate: the current bucket becomes previous, start fresh.
		c.previous = c.current
		c.current = 0
		c.bucketT = time.Now()
	}
	return c.previous
}

// countingConn wraps a net.Conn and counts bytes written.
type countingConn struct {
	net.Conn
	sent byteCounter
}

func newCountingConn(c net.Conn) *countingConn {
	return &countingConn{Conn: c}
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.sent.add(n)
	return n, err
}
