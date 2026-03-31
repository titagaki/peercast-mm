package servent

import (
	"bufio"
	"net"
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
)

// newTestOutputStream は net.Pipe を使って PCPOutputStream とピア接続を返す。
// テスト終了時に peer.Close() と out.Close() を呼ぶこと。
func newTestOutputStream(t *testing.T) (*PCPOutputStream, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	cc := newCountingConn(c1)
	br := bufio.NewReader(cc)
	var sid pcp.GnuID
	out := newPCPOutputStream(cc, br, sid, channel.New(pcp.GnuID{}, pcp.GnuID{}, 0), 42)
	return out, c2
}

// --- notify ---

// TestNotify_NonBlocking は最初の通知がチャンネルに届き、2 回目がブロックしないことを確認する。
func TestNotify_NonBlocking(t *testing.T) {
	ch := make(chan struct{}, 1)
	notify(ch)
	notify(ch) // バッファ満杯でもブロックしない
	if len(ch) != 1 {
		t.Errorf("channel length: got %d, want 1", len(ch))
	}
}

// --- ipToUint32 ---

// TestIPToUint32 は IPv4 アドレスを uint32 へ正しく変換することを確認する。
func TestIPToUint32(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want uint32
	}{
		{
			name: "1.2.3.4",
			addr: &net.TCPAddr{IP: net.IP{1, 2, 3, 4}},
			want: 0x01020304,
		},
		{
			name: "192.168.1.100",
			addr: &net.TCPAddr{IP: net.IPv4(192, 168, 1, 100)},
			want: 0xC0A80164,
		},
		{
			name: "nil IP",
			addr: &net.TCPAddr{IP: nil},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ipToUint32(tt.addr)
			if got != tt.want {
				t.Errorf("ipToUint32: got 0x%08X, want 0x%08X", got, tt.want)
			}
		})
	}
}

// TestIPToUint32_NonTCPAddr は *net.TCPAddr 以外で 0 を返すことを確認する。
func TestIPToUint32_NonTCPAddr(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IP{1, 2, 3, 4}}
	if got := ipToUint32(addr); got != 0 {
		t.Errorf("ipToUint32(UDP): got %d, want 0", got)
	}
}

// --- Atom builders ---

// TestBuildChanInfoAtom は PCPChanInfo タグを持つアトムが生成されることを確認する。
func TestBuildChanInfoAtom(t *testing.T) {
	info := channel.ChannelInfo{Name: "test", Genre: "music", Bitrate: 128}
	a := buildChanInfoAtom(info)
	if a.Tag != pcp.PCPChanInfo {
		t.Errorf("Tag: got %v, want PCPChanInfo", a.Tag)
	}
}

// TestBuildChanTrackAtom は PCPChanTrack タグを持つアトムが生成されることを確認する。
func TestBuildChanTrackAtom(t *testing.T) {
	track := channel.TrackInfo{Title: "My Track", Creator: "Artist"}
	a := buildChanTrackAtom(track)
	if a.Tag != pcp.PCPChanTrack {
		t.Errorf("Tag: got %v, want PCPChanTrack", a.Tag)
	}
}

// TestBuildPktHeadAtom は PCPChanPkt タグを持つアトムが生成されることを確認する。
func TestBuildPktHeadAtom(t *testing.T) {
	a := buildPktHeadAtom([]byte{0x01, 0x02}, 42)
	if a.Tag != pcp.PCPChanPkt {
		t.Errorf("Tag: got %v, want PCPChanPkt", a.Tag)
	}
}

// TestBuildChanAtom はトップレベルが PCPChan タグを持つアトムが生成されることを確認する。
func TestBuildChanAtom(t *testing.T) {
	var id pcp.GnuID
	a := buildChanAtom(id, id, channel.ChannelInfo{Name: "test"}, channel.TrackInfo{}, []byte{0x01}, 0)
	if a.Tag != pcp.PCPChan {
		t.Errorf("Tag: got %v, want PCPChan", a.Tag)
	}
}

// --- PCPOutputStream ---

// TestPCPOutputStream_Type は Type() が OutputStreamPCP を返すことを確認する。
func TestPCPOutputStream_Type(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	if got := out.Type(); got != channel.OutputStreamPCP {
		t.Errorf("Type: got %v, want OutputStreamPCP", got)
	}
}

// TestPCPOutputStream_ID は ID() がコンストラクタで指定した値を返すことを確認する。
func TestPCPOutputStream_ID(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	if got := out.ID(); got != 42 {
		t.Errorf("ID: got %d, want 42", got)
	}
}

// TestPCPOutputStream_RemoteAddr は RemoteAddr() が空でない文字列を返すことを確認する。
func TestPCPOutputStream_RemoteAddr(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	if got := out.RemoteAddr(); got == "" {
		t.Error("RemoteAddr: got empty string")
	}
}

// TestPCPOutputStream_SendRate_Initial は初期 SendRate が 0 であることを確認する。
func TestPCPOutputStream_SendRate_Initial(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	if got := out.SendRate(); got != 0 {
		t.Errorf("SendRate (initial): got %d, want 0", got)
	}
}

// TestPCPOutputStream_NotifyHeader は headerCh にシグナルが送られることを確認する。
func TestPCPOutputStream_NotifyHeader(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	out.NotifyHeader()
	if len(out.headerCh) != 1 {
		t.Error("NotifyHeader: headerCh not signaled")
	}
}

// TestPCPOutputStream_NotifyInfo は infoCh にシグナルが送られることを確認する。
func TestPCPOutputStream_NotifyInfo(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	out.NotifyInfo()
	if len(out.infoCh) != 1 {
		t.Error("NotifyInfo: infoCh not signaled")
	}
}

// TestPCPOutputStream_NotifyTrack は trackCh にシグナルが送られることを確認する。
func TestPCPOutputStream_NotifyTrack(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()
	defer out.Close()

	out.NotifyTrack()
	if len(out.trackCh) != 1 {
		t.Error("NotifyTrack: trackCh not signaled")
	}
}

// TestPCPOutputStream_Close_Idempotent は Close が冪等でパニックしないことを確認する。
func TestPCPOutputStream_Close_Idempotent(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()

	out.Close()
	out.Close() // 2 回目はパニックしない

	out.mu.Lock()
	closed := out.closed
	out.mu.Unlock()
	if !closed {
		t.Error("closed: expected true after Close")
	}
}

// TestPCPOutputStream_Close_SignalsCloseCh は Close が closeCh を閉じることを確認する。
func TestPCPOutputStream_Close_SignalsCloseCh(t *testing.T) {
	out, peer := newTestOutputStream(t)
	defer peer.Close()

	out.Close()

	select {
	case <-out.closeCh:
		// OK
	default:
		t.Error("closeCh: expected closed after Close")
	}
}
