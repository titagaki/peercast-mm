package channel

import (
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"
)

// mockOutput は テスト用の OutputStream スタブ。
type mockOutput struct {
	t OutputStreamType
}

func (m *mockOutput) NotifyHeader() {}
func (m *mockOutput) NotifyInfo()   {}
func (m *mockOutput) NotifyTrack()  {}
func (m *mockOutput) Close()        {}
func (m *mockOutput) Type() OutputStreamType { return m.t }

func newPCPOut() *mockOutput  { return &mockOutput{OutputStreamPCP} }
func newHTTPOut() *mockOutput { return &mockOutput{OutputStreamHTTP} }

func newTestChannel() *Channel {
	return New(pcp.GnuID{}, pcp.GnuID{})
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
