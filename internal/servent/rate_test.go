package servent

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestByteCounter_InitialRateZero は初期状態で rate() が 0 を返すことを確認する。
func TestByteCounter_InitialRateZero(t *testing.T) {
	var c byteCounter
	if got := c.rate(); got != 0 {
		t.Errorf("rate (initial): got %d, want 0", got)
	}
}

// TestByteCounter_AddAccumulates は add したバイト数が current に積算されることを確認する。
func TestByteCounter_AddAccumulates(t *testing.T) {
	var c byteCounter
	c.add(100)
	c.add(200)
	c.mu.Lock()
	got := c.current
	c.mu.Unlock()
	if got != 300 {
		t.Errorf("current after add(100)+add(200): got %d, want 300", got)
	}
}

// TestByteCounter_AddIgnoresNonPositive は add(0) / add(-1) が無視されることを確認する。
func TestByteCounter_AddIgnoresNonPositive(t *testing.T) {
	var c byteCounter
	c.add(0)
	c.add(-1)
	c.mu.Lock()
	got := c.current
	c.mu.Unlock()
	if got != 0 {
		t.Errorf("current after add(0)+add(-1): got %d, want 0", got)
	}
}

// TestByteCounter_RateAfterBucketRollover は前のバケットのバイト数が rate() で返ることを確認する。
func TestByteCounter_RateAfterBucketRollover(t *testing.T) {
	var c byteCounter
	c.add(500)

	// bucketT を 2 秒前にずらして次の add でロールオーバーを強制する。
	c.mu.Lock()
	c.bucketT = time.Now().Add(-2 * time.Second)
	c.mu.Unlock()

	c.add(1) // ロールオーバー: previous=500, current=1

	if got := c.rate(); got != 500 {
		t.Errorf("rate after rollover: got %d, want 500", got)
	}
}

// TestByteCounter_RateWithinFirstSecond は最初のバケット内では rate() が 0 を返すことを確認する。
// (previous がまだ 0 のため)
func TestByteCounter_RateWithinFirstSecond(t *testing.T) {
	var c byteCounter
	c.add(100)
	if got := c.rate(); got != 0 {
		t.Errorf("rate (within first second): got %d, want 0", got)
	}
}

// TestByteCounter_RateStaleReturnsZero は最後の add から 2 秒以上経過すると rate() が 0 を返すことを確認する。
func TestByteCounter_RateStaleReturnsZero(t *testing.T) {
	var c byteCounter
	c.mu.Lock()
	c.previous = 999
	c.bucketT = time.Now().Add(-3 * time.Second) // 3 秒前: stale
	c.mu.Unlock()

	if got := c.rate(); got != 0 {
		t.Errorf("rate (stale): got %d, want 0", got)
	}
}

// TestByteCounter_Concurrent は並行 add / rate でデータ競合が起きないことを確認する。
func TestByteCounter_Concurrent(t *testing.T) {
	var c byteCounter
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.add(1)
				c.rate()
			}
		}()
	}
	wg.Wait()
}

// TestCountingConn_Write は Write が送信バイト数を正確にカウントすることを確認する。
func TestCountingConn_Write(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	cc := newCountingConn(c1)

	// 反対側を drain しないと Write がブロックする。
	go io.Copy(io.Discard, c2)

	cc.Write([]byte("hello"))
	cc.Write([]byte("world"))
	cc.Close()

	cc.sent.mu.Lock()
	got := cc.sent.current
	cc.sent.mu.Unlock()
	if got != 10 {
		t.Errorf("bytes counted: got %d, want 10", got)
	}
}
