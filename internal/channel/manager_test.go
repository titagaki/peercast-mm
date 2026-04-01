package channel

import (
	"fmt"
	"sync"
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"
)

// newTestManager は broadcastID = ゼロ値の Manager を返す。
func newTestManager() *Manager {
	return NewManager(pcp.GnuID{})
}

// sampleInfo / sampleTrack はテスト用のダミーメタデータ。
func sampleInfo() ChannelInfo { return ChannelInfo{Name: "test", Genre: "music"} }
func sampleTrack() TrackInfo  { return TrackInfo{Title: "track1"} }

// issueKey はテスト用のヘルパー。accountName と streamKey を登録する。
func issueKey(t *testing.T, m *Manager, accountName, streamKey string) {
	t.Helper()
	if err := m.IssueStreamKey(accountName, streamKey); err != nil {
		t.Fatalf("IssueStreamKey(%q, %q): unexpected error: %v", accountName, streamKey, err)
	}
}

// --- IssueStreamKey / IsIssuedKey ---

// TestManager_IssueStreamKey は登録したキーが有効になることを確認する。
func TestManager_IssueStreamKey(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_abc")
	if !m.IsIssuedKey("sk_abc") {
		t.Error("IsIssuedKey: got false, want true")
	}
}

// TestManager_IssueStreamKey_Overwrites は同じアカウント名で再登録すると
// 旧キーが無効化され新キーが有効になることを確認する。
func TestManager_IssueStreamKey_Overwrites(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_old")
	issueKey(t, m, "user1", "sk_new")

	if m.IsIssuedKey("sk_old") {
		t.Error("IsIssuedKey(old): got true after overwrite, want false")
	}
	if !m.IsIssuedKey("sk_new") {
		t.Error("IsIssuedKey(new): got false, want true")
	}
}

// TestManager_IsIssuedKey は発行済みキーのみ true を返すことを確認する。
func TestManager_IsIssuedKey(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	if !m.IsIssuedKey("sk_test") {
		t.Error("IsIssuedKey: got false, want true")
	}
	if m.IsIssuedKey("sk_notissued") {
		t.Error("IsIssuedKey(unknown): got true, want false")
	}
}

// --- RevokeStreamKey ---

// TestManager_RevokeStreamKey_Success は Revoke 後にキーが無効になることを確認する。
func TestManager_RevokeStreamKey_Success(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")

	if ok := m.RevokeStreamKey("user1"); !ok {
		t.Error("RevokeStreamKey: got false, want true")
	}
	if m.IsIssuedKey("sk_test") {
		t.Error("IsIssuedKey after revoke: got true, want false")
	}
}

// TestManager_RevokeStreamKey_NotFound は未登録アカウントで false を返すことを確認する。
func TestManager_RevokeStreamKey_NotFound(t *testing.T) {
	m := newTestManager()
	if ok := m.RevokeStreamKey("nobody"); ok {
		t.Error("RevokeStreamKey(unknown): got true, want false")
	}
}

// TestManager_RevokeStreamKey_ChannelStillActive は Revoke 後も放送中チャンネルが
// 停止されないことを確認する。
func TestManager_RevokeStreamKey_ChannelStillActive(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	m.RevokeStreamKey("user1")

	if _, found := m.GetByID(ch.ID); !found {
		t.Error("channel should still be active after RevokeStreamKey")
	}
}

// --- Broadcast ---

// TestManager_Broadcast_Success はブロードキャスト開始が成功することを確認する。
func TestManager_Broadcast_Success(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")

	ch, err := m.Broadcast("sk_test", sampleInfo(), sampleTrack())
	if err != nil {
		t.Fatalf("Broadcast: unexpected error: %v", err)
	}
	if ch == nil {
		t.Fatal("Broadcast: got nil channel")
	}
}

// TestManager_Broadcast_UnissuedKey は未発行キーでエラーになることを確認する。
func TestManager_Broadcast_UnissuedKey(t *testing.T) {
	m := newTestManager()
	_, err := m.Broadcast("sk_unknown", sampleInfo(), sampleTrack())
	if err == nil {
		t.Error("Broadcast(unissued key): expected error, got nil")
	}
}

// TestManager_Broadcast_DuplicateKey は同じキーで 2 回目の Broadcast がエラーになることを確認する。
func TestManager_Broadcast_DuplicateKey(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	_, err := m.Broadcast("sk_test", sampleInfo(), sampleTrack())
	if err == nil {
		t.Error("Broadcast(duplicate key): expected error, got nil")
	}
}

// TestManager_Broadcast_SetsInfoTrack は Broadcast 後にチャンネルの Info/Track が
// 指定した値になっていることを確認する。
func TestManager_Broadcast_SetsInfoTrack(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	info := sampleInfo()
	track := sampleTrack()
	ch, _ := m.Broadcast("sk_test", info, track)

	if ch.Info() != info {
		t.Errorf("Channel.Info: got %+v, want %+v", ch.Info(), info)
	}
	if ch.Track() != track {
		t.Errorf("Channel.Track: got %+v, want %+v", ch.Track(), track)
	}
}

// --- Stop ---

// TestManager_Stop_Success は Stop が true を返しチャンネルを削除することを確認する。
func TestManager_Stop_Success(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	if ok := m.Stop(ch.ID); !ok {
		t.Error("Stop: got false, want true")
	}
	if _, found := m.GetByID(ch.ID); found {
		t.Error("GetByID after Stop: expected not found")
	}
	if _, found := m.GetByStreamKey("sk_test"); found {
		t.Error("GetByStreamKey after Stop: expected not found")
	}
}

// TestManager_Stop_NotFound は存在しない ID で Stop が false を返すことを確認する。
func TestManager_Stop_NotFound(t *testing.T) {
	m := newTestManager()
	if ok := m.Stop(pcp.GnuID{}); ok {
		t.Error("Stop(unknown ID): got true, want false")
	}
}

// TestManager_Stop_StreamKeyRemainsValid は Stop 後もストリームキーが有効で
// 再 Broadcast できることを確認する。
func TestManager_Stop_StreamKeyRemainsValid(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())
	m.Stop(ch.ID)

	if !m.IsIssuedKey("sk_test") {
		t.Error("IsIssuedKey after Stop: got false, stream key should remain valid")
	}
	if _, err := m.Broadcast("sk_test", sampleInfo(), sampleTrack()); err != nil {
		t.Errorf("Broadcast after Stop: unexpected error: %v", err)
	}
}

// TestManager_Stop_DeterministicChannelID は同じ引数で再 Broadcast すると
// 同じチャンネル ID が生成されることを確認する。
func TestManager_Stop_DeterministicChannelID(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	info := sampleInfo()
	track := sampleTrack()

	ch1, _ := m.Broadcast("sk_test", info, track)
	id1 := ch1.ID
	m.Stop(id1)

	ch2, _ := m.Broadcast("sk_test", info, track)
	if ch2.ID != id1 {
		t.Errorf("channel ID not deterministic: first=%v second=%v", id1, ch2.ID)
	}
}

// --- StopAll ---

// TestManager_StopAll は全チャンネルを停止しキーを保持することを確認する。
func TestManager_StopAll(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_1")
	issueKey(t, m, "user2", "sk_2")
	m.Broadcast("sk_1", sampleInfo(), sampleTrack())
	m.Broadcast("sk_2", sampleInfo(), sampleTrack())

	m.StopAll()

	if list := m.List(); len(list) != 0 {
		t.Errorf("List after StopAll: got %d channels, want 0", len(list))
	}
	if !m.IsIssuedKey("sk_1") || !m.IsIssuedKey("sk_2") {
		t.Error("IsIssuedKey after StopAll: stream keys should remain valid")
	}
}

// --- GetByStreamKey / GetByID / StreamKeyByID ---

// TestManager_GetByStreamKey はブロードキャスト中のチャンネルを取得できることを確認する。
func TestManager_GetByStreamKey(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	got, ok := m.GetByStreamKey("sk_test")
	if !ok {
		t.Fatal("GetByStreamKey: got false, want true")
	}
	if got != ch {
		t.Error("GetByStreamKey: returned different channel pointer")
	}
}

// TestManager_GetByStreamKey_NotFound は未登録キーで false が返ることを確認する。
func TestManager_GetByStreamKey_NotFound(t *testing.T) {
	m := newTestManager()
	_, ok := m.GetByStreamKey("sk_unknown")
	if ok {
		t.Error("GetByStreamKey(unknown): got true, want false")
	}
}

// TestManager_GetByID はチャンネル ID でチャンネルを取得できることを確認する。
func TestManager_GetByID(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	got, ok := m.GetByID(ch.ID)
	if !ok {
		t.Fatal("GetByID: got false, want true")
	}
	if got != ch {
		t.Error("GetByID: returned different channel pointer")
	}
}

// TestManager_StreamKeyByID はチャンネル ID からストリームキーを取得できることを確認する。
func TestManager_StreamKeyByID(t *testing.T) {
	m := newTestManager()
	issueKey(t, m, "user1", "sk_test")
	ch, _ := m.Broadcast("sk_test", sampleInfo(), sampleTrack())

	got, ok := m.StreamKeyByID(ch.ID)
	if !ok {
		t.Fatal("StreamKeyByID: got false, want true")
	}
	if got != "sk_test" {
		t.Errorf("StreamKeyByID: got %q, want %q", got, "sk_test")
	}
}

// --- List ---

// TestManager_List は全アクティブチャンネルのスナップショットを返すことを確認する。
func TestManager_List(t *testing.T) {
	m := newTestManager()
	if list := m.List(); len(list) != 0 {
		t.Errorf("List (empty): got %d, want 0", len(list))
	}

	issueKey(t, m, "user1", "sk_1")
	issueKey(t, m, "user2", "sk_2")
	ch1, _ := m.Broadcast("sk_1", sampleInfo(), sampleTrack())
	ch2, _ := m.Broadcast("sk_2", sampleInfo(), sampleTrack())

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("List: got %d, want 2", len(list))
	}

	found := map[pcp.GnuID]bool{ch1.ID: false, ch2.ID: false}
	for _, ch := range list {
		found[ch.ID] = true
	}
	for id, ok := range found {
		if !ok {
			t.Errorf("List: channel %v not found", id)
		}
	}
}

// --- Concurrent access ---

// TestManager_Concurrent は並行 IssueStreamKey / Broadcast / Stop でデータ競合が
// 起きないことを確認する (go test -race で検出)。
func TestManager_Concurrent(t *testing.T) {
	m := newTestManager()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		i := i
		key := fmt.Sprintf("sk_%d", i)
		account := fmt.Sprintf("user%d", i)
		issueKey(t, m, account, key)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := m.Broadcast(key, sampleInfo(), sampleTrack())
			if err != nil {
				return
			}
			m.GetByStreamKey(key)
			m.GetByID(ch.ID)
			m.StreamKeyByID(ch.ID)
			m.List()
			m.Stop(ch.ID)
		}()
	}
	wg.Wait()
}
