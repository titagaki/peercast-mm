package channel

import (
	"bytes"
	"sync"
	"testing"
)

// TestContentBufferSizeForBitrate はビットレートと秒数からバッファサイズを
// 正しく計算できることを確認する。
func TestContentBufferSizeForBitrate(t *testing.T) {
	tests := []struct {
		name    string
		kbps    uint32
		seconds float64
		wantMin int
	}{
		{"zero bitrate returns default", 0, 8, DefaultContentBufferSize},
		{"zero seconds returns default", 1000, 0, DefaultContentBufferSize},
		{"low bitrate clamps to default", 100, 1, DefaultContentBufferSize},
		{"4Mbps 8s", 4000, 8, 200},
		{"8Mbps 8s", 8000, 8, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContentBufferSizeForBitrate(tt.kbps, tt.seconds)
			if got < tt.wantMin {
				t.Errorf("ContentBufferSizeForBitrate(%d, %f) = %d, want >= %d", tt.kbps, tt.seconds, got, tt.wantMin)
			}
		})
	}
}

// TestContentBuffer_HeaderEmpty はヘッダー未設定時に nil と pos=0 が返ることを確認する。
func TestContentBuffer_HeaderEmpty(t *testing.T) {
	var b ContentBuffer
	data, pos := b.Header()
	if data != nil {
		t.Errorf("Header: got %v, want nil", data)
	}
	if pos != 0 {
		t.Errorf("Header pos: got %d, want 0", pos)
	}
}

// TestContentBuffer_SetHeaderCopies は SetHeader がデータをコピーし、
// 元のスライスを変更しても影響を受けないことを確認する。
func TestContentBuffer_SetHeaderCopies(t *testing.T) {
	var b ContentBuffer
	orig := []byte{0x01, 0x02}
	b.SetHeader(orig)
	orig[0] = 0xFF

	got, _ := b.Header()
	if got[0] != 0x01 {
		t.Errorf("SetHeader did not copy: got 0x%02X, want 0x01", got[0])
	}
}

// TestContentBuffer_HeaderPosAfterWrite は Write 後の SetHeader で
// headerPos が最後のパケットの末尾を指すことを確認する。
func TestContentBuffer_HeaderPosAfterWrite(t *testing.T) {
	var b ContentBuffer
	b.Write([]byte{0xAA, 0xBB, 0xCC}, 100, 0) // pos=100, len=3 → 次のpos=103
	b.SetHeader([]byte{0xFF})
	_, pos := b.Header()
	if pos != 103 {
		t.Errorf("headerPos: got %d, want 103", pos)
	}
}

// TestContentBuffer_HasData は Write 前後の HasData を確認する。
func TestContentBuffer_HasData(t *testing.T) {
	var b ContentBuffer
	if b.HasData() {
		t.Error("HasData: expected false for empty buffer")
	}
	b.Write([]byte{0x01}, 0, 0)
	if !b.HasData() {
		t.Error("HasData: expected true after Write")
	}
}

// TestContentBuffer_OldestNewestPos_Empty は空バッファで 0 が返ることを確認する。
func TestContentBuffer_OldestNewestPos_Empty(t *testing.T) {
	var b ContentBuffer
	if b.OldestPos() != 0 {
		t.Errorf("OldestPos empty: got %d, want 0", b.OldestPos())
	}
	if b.NewestPos() != 0 {
		t.Errorf("NewestPos empty: got %d, want 0", b.NewestPos())
	}
}

// TestContentBuffer_OldestNewestPos_FewPackets はバッファサイズ未満のパケット数での
// OldestPos / NewestPos を確認する。
func TestContentBuffer_OldestNewestPos_FewPackets(t *testing.T) {
	var b ContentBuffer
	b.Write([]byte{0x01}, 10, 0)
	b.Write([]byte{0x02}, 20, 0x02)
	b.Write([]byte{0x03}, 30, 0x02)

	if got := b.OldestPos(); got != 10 {
		t.Errorf("OldestPos: got %d, want 10", got)
	}
	if got := b.NewestPos(); got != 30 {
		t.Errorf("NewestPos: got %d, want 30", got)
	}
}

// TestContentBuffer_RingOverflow はリングバッファが一周した後の
// OldestPos / NewestPos を確認する。
func TestContentBuffer_RingOverflow(t *testing.T) {
	var b ContentBuffer
	// DefaultContentBufferSize+1 個書き込む。先頭 1 件が追い出される。
	for i := 0; i < DefaultContentBufferSize+1; i++ {
		b.Write([]byte{byte(i)}, uint32(i * 10), func() byte { if i > 0 { return 0x02 }; return 0 }())
	}
	// インデックス 1 が最古になるはず (インデックス 0 は上書きされた)
	if got := b.OldestPos(); got != 10 {
		t.Errorf("OldestPos after overflow: got %d, want 10", got)
	}
	want := uint32(DefaultContentBufferSize * 10)
	if got := b.NewestPos(); got != want {
		t.Errorf("NewestPos after overflow: got %d, want %d", got, want)
	}
}

// TestContentBuffer_Since_Empty は空バッファで nil を返すことを確認する。
func TestContentBuffer_Since_Empty(t *testing.T) {
	var b ContentBuffer
	if got := b.Since(0); got != nil {
		t.Errorf("Since on empty: got %v, want nil", got)
	}
}

// TestContentBuffer_Since_Basic は基本的な Since の動作を確認する。
func TestContentBuffer_Since_Basic(t *testing.T) {
	var b ContentBuffer
	b.Write([]byte{0x01}, 0, 0)
	b.Write([]byte{0x02}, 10, 0x02)
	b.Write([]byte{0x03}, 20, 0x02)

	got := b.Since(10)
	if len(got) != 2 {
		t.Fatalf("Since(10): got %d packets, want 2", len(got))
	}
	if got[0].Pos != 10 || got[1].Pos != 20 {
		t.Errorf("Since(10): unexpected positions %v", got)
	}
}

// TestContentBuffer_Since_OlderThanOldest は要求 pos がバッファ最古より古い場合、
// 先頭から返すことを確認する。
func TestContentBuffer_Since_OlderThanOldest(t *testing.T) {
	var b ContentBuffer
	b.Write([]byte{0x01}, 100, 0)
	b.Write([]byte{0x02}, 200, 0x02)

	got := b.Since(0)
	if len(got) != 2 {
		t.Fatalf("Since(0): got %d packets, want 2", len(got))
	}
}

// TestContentBuffer_Since_AfterNewest は最新より大きい pos で nil を返すことを確認する。
func TestContentBuffer_Since_AfterNewest(t *testing.T) {
	var b ContentBuffer
	b.Write([]byte{0x01}, 0, 0)
	b.Write([]byte{0x02}, 10, 0x02)

	got := b.Since(100)
	if got != nil {
		t.Errorf("Since(100): got %v, want nil", got)
	}
}

// TestContentBuffer_Since_RingOverflow はリングバッファが一周した後の
// Since が正しく動作することを確認する。
func TestContentBuffer_Since_RingOverflow(t *testing.T) {
	var b ContentBuffer
	for i := 0; i < DefaultContentBufferSize+10; i++ {
		b.Write([]byte{byte(i)}, uint32(i), func() byte { if i > 0 { return 0x02 }; return 0 }())
	}
	oldest := b.OldestPos()
	got := b.Since(oldest)
	if len(got) != DefaultContentBufferSize {
		t.Errorf("Since(oldest) after overflow: got %d packets, want %d", len(got), DefaultContentBufferSize)
	}
}

// TestContentBuffer_WriteCopies は Write がデータをコピーし、
// 元のスライスを変更しても影響を受けないことを確認する。
func TestContentBuffer_WriteCopies(t *testing.T) {
	var b ContentBuffer
	data := []byte{0xAA, 0xBB}
	b.Write(data, 0, 0)
	data[0] = 0xFF

	got := b.Since(0)
	if !bytes.Equal(got[0].Data, []byte{0xAA, 0xBB}) {
		t.Errorf("Write did not copy: got %v", got[0].Data)
	}
}

// TestContentBuffer_ConcurrentWriteRead は並行 Write / Since / Header で
// データ競合が起きないことを確認する (go test -race で検出)。
func TestContentBuffer_ConcurrentWriteRead(t *testing.T) {
	var b ContentBuffer
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				b.Write([]byte{byte(n), byte(j)}, uint32(n*100+j), func() byte { if j > 0 { return 0x02 }; return 0 }())
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			b.SetHeader([]byte{byte(j)})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			b.Since(0)
			b.Header()
			b.OldestPos()
			b.NewestPos()
			b.HasData()
		}
	}()

	wg.Wait()
}
