package channel

import (
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"
)

// mockOutput は テスト用の OutputStream スタブ。
type mockOutput struct {
	t  OutputStreamType
	id int
}

func (m *mockOutput) NotifyHeader()              {}
func (m *mockOutput) NotifyInfo()                {}
func (m *mockOutput) NotifyTrack()               {}
func (m *mockOutput) Close()                     {}
func (m *mockOutput) Type() OutputStreamType     { return m.t }
func (m *mockOutput) ID() int                    { return m.id }
func (m *mockOutput) RemoteAddr() string         { return "127.0.0.1:0" }
func (m *mockOutput) SendRate() int64            { return 0 }
func (m *mockOutput) SendBcst(_ *pcp.Atom)       {}

var nextMockID int

func newPCPOut() *mockOutput  { nextMockID++; return &mockOutput{t: OutputStreamPCP, id: nextMockID} }
func newHTTPOut() *mockOutput { nextMockID++; return &mockOutput{t: OutputStreamHTTP, id: nextMockID} }

func newTestChannel() *Channel {
	return New(pcp.GnuID{}, pcp.GnuID{}, 0)
}

// TestChannel_AddOutputCounters は AddOutput がカウンタを正しく増加させることを確認する。
func TestChannel_AddOutputCounters(t *testing.T) {
	ch := newTestChannel()

	ch.AddOutput(newPCPOut())
	ch.AddOutput(newPCPOut())
	ch.AddOutput(newHTTPOut())

	if got := ch.NumRelays(); got != 2 {
		t.Errorf("NumRelays: got %d, want 2", got)
	}
	if got := ch.NumListeners(); got != 1 {
		t.Errorf("NumListeners: got %d, want 1", got)
	}
}

// TestChannel_RemoveOutputCounters は RemoveOutput がカウンタを正しく減少させることを確認する。
func TestChannel_RemoveOutputCounters(t *testing.T) {
	ch := newTestChannel()

	pcp1 := newPCPOut()
	pcp2 := newPCPOut()
	http1 := newHTTPOut()

	ch.AddOutput(pcp1)
	ch.AddOutput(pcp2)
	ch.AddOutput(http1)

	ch.RemoveOutput(pcp1)

	if got := ch.NumRelays(); got != 1 {
		t.Errorf("NumRelays after remove: got %d, want 1", got)
	}
	if got := ch.NumListeners(); got != 1 {
		t.Errorf("NumListeners after remove: got %d, want 1", got)
	}

	ch.RemoveOutput(http1)

	if got := ch.NumListeners(); got != 0 {
		t.Errorf("NumListeners after remove: got %d, want 0", got)
	}
}

// TestChannel_RemoveUnknownOutput は未登録の OutputStream を RemoveOutput しても
// カウンタが負にならないことを確認する。
func TestChannel_RemoveUnknownOutput(t *testing.T) {
	ch := newTestChannel()
	ch.AddOutput(newPCPOut())

	unknown := newPCPOut() // 追加していない
	ch.RemoveOutput(unknown)

	if got := ch.NumRelays(); got != 1 {
		t.Errorf("NumRelays: got %d, want 1 (unknown remove should be no-op)", got)
	}
}

// TestChannel_CloseAllResetsCounters は CloseAll がカウンタをゼロにリセットすることを確認する。
// RTMP 切断後に YP クライアントが次の bcst を送るタイミングで numl/numr が
// 古い値のまま送信されないことが目的。
func TestChannel_CloseAllResetsCounters(t *testing.T) {
	ch := newTestChannel()

	ch.AddOutput(newPCPOut())
	ch.AddOutput(newPCPOut())
	ch.AddOutput(newHTTPOut())

	ch.CloseAll()

	if got := ch.NumRelays(); got != 0 {
		t.Errorf("NumRelays after CloseAll: got %d, want 0", got)
	}
	if got := ch.NumListeners(); got != 0 {
		t.Errorf("NumListeners after CloseAll: got %d, want 0", got)
	}
}

// TestChannel_TryAddOutput_WithinLimit は上限内では TryAddOutput が true を返すことを確認する。
func TestChannel_TryAddOutput_WithinLimit(t *testing.T) {
	ch := newTestChannel()

	if ok := ch.TryAddOutput(newPCPOut(), 2, 0); !ok {
		t.Fatal("TryAddOutput: expected true for first relay")
	}
	if ok := ch.TryAddOutput(newPCPOut(), 2, 0); !ok {
		t.Fatal("TryAddOutput: expected true for second relay")
	}
	if got := ch.NumRelays(); got != 2 {
		t.Errorf("NumRelays: got %d, want 2", got)
	}
}

// TestChannel_TryAddOutput_LimitReached は上限到達時に TryAddOutput が false を返し、
// カウンタが増加しないことを確認する。
func TestChannel_TryAddOutput_LimitReached(t *testing.T) {
	ch := newTestChannel()

	ch.TryAddOutput(newPCPOut(), 1, 0)

	if ok := ch.TryAddOutput(newPCPOut(), 1, 0); ok {
		t.Fatal("TryAddOutput: expected false when relay limit reached")
	}
	if got := ch.NumRelays(); got != 1 {
		t.Errorf("NumRelays: got %d, want 1 (rejected connection must not increment counter)", got)
	}
}

// TestChannel_TryAddOutput_Unlimited は maxRelays=0 のとき上限なしで追加できることを確認する。
func TestChannel_TryAddOutput_Unlimited(t *testing.T) {
	ch := newTestChannel()

	for i := 0; i < 10; i++ {
		if ok := ch.TryAddOutput(newPCPOut(), 0, 0); !ok {
			t.Fatalf("TryAddOutput: expected true for relay %d with unlimited setting", i+1)
		}
	}
	if got := ch.NumRelays(); got != 10 {
		t.Errorf("NumRelays: got %d, want 10", got)
	}
}

// TestChannel_IsRelayFull はリレー上限判定が正しく動作することを確認する。
func TestChannel_IsRelayFull(t *testing.T) {
	ch := newTestChannel()

	if ch.IsRelayFull(2) {
		t.Error("IsRelayFull: expected false when no relays connected")
	}
	ch.TryAddOutput(newPCPOut(), 0, 0)
	if ch.IsRelayFull(2) {
		t.Error("IsRelayFull: expected false when 1 of 2 slots used")
	}
	ch.TryAddOutput(newPCPOut(), 0, 0)
	if !ch.IsRelayFull(2) {
		t.Error("IsRelayFull: expected true when 2 of 2 slots used")
	}
	if ch.IsRelayFull(0) {
		t.Error("IsRelayFull: expected false when maxRelays=0 (unlimited)")
	}
}

// TestChannel_CountersAfterCloseAllAndReadd は CloseAll 後に再登録しても
// カウンタが正しく動作することを確認する。
func TestChannel_CountersAfterCloseAllAndReadd(t *testing.T) {
	ch := newTestChannel()

	ch.AddOutput(newPCPOut())
	ch.CloseAll()

	ch.AddOutput(newHTTPOut())

	if got := ch.NumRelays(); got != 0 {
		t.Errorf("NumRelays: got %d, want 0", got)
	}
	if got := ch.NumListeners(); got != 1 {
		t.Errorf("NumListeners: got %d, want 1", got)
	}
}
