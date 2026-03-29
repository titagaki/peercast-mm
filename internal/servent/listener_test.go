package servent

import (
	"bufio"
	"net"
	"testing"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/version"
)

// writePCPMagic は "pcp\n" ハンドシェイク冒頭の 12 バイト (tag+length+version) を書き込む。
func writePCPMagic(conn net.Conn) error {
	return pcp.NewIntAtom(pcp.NewID4("pcp\n"), version.PCPVersion).Write(conn)
}

// pingResult は client goroutine が収集した応答を保持する。
type pingResult struct {
	writeErr error
	oleh     *pcp.Atom
	quit     *pcp.Atom
	readErr  error
}

// runPingClient は net.Pipe の client 側で「送信 → 受信」を 1 goroutine 内で行い
// 結果をチャネルに返す。net.Pipe はブロッキング同期なので、送信と受信を同一 goroutine に
// まとめることでデッドロックを回避する。
func runPingClient(client net.Conn, clientID, sessionID pcp.GnuID) <-chan pingResult {
	ch := make(chan pingResult, 1)
	go func() {
		var r pingResult
		defer client.Close()

		if err := writePCPMagic(client); err != nil {
			r.writeErr = err
			ch <- r
			return
		}
		helo := pcp.NewParentAtom(pcp.PCPHelo,
			pcp.NewIDAtom(pcp.PCPHeloSessionID, clientID),
			pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
		)
		if err := helo.Write(client); err != nil {
			r.writeErr = err
			ch <- r
			return
		}

		// handlePing がレスポンスを書き込んでいる間、ここで読み取る。
		oleh, err := pcp.ReadAtom(client)
		if err != nil {
			r.readErr = err
			ch <- r
			return
		}
		r.oleh = oleh

		quit, err := pcp.ReadAtom(client)
		if err != nil {
			r.readErr = err
			ch <- r
			return
		}
		r.quit = quit
		ch <- r
	}()
	return ch
}

// TestHandlePing_SendsOlehAndQuit は handlePing が oleh(sid) + quit を返すことを確認する。
func TestHandlePing_SendsOlehAndQuit(t *testing.T) {
	client, server := net.Pipe()

	sessionID := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	clientID := pcp.GnuID{17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}

	resultCh := runPingClient(client, clientID, sessionID)

	br := bufio.NewReader(server)
	handlePing(server, br, sessionID)

	r := <-resultCh

	if r.writeErr != nil {
		t.Fatalf("client write error: %v", r.writeErr)
	}
	if r.readErr != nil {
		t.Fatalf("client read error: %v", r.readErr)
	}

	if r.oleh == nil || r.oleh.Tag != pcp.PCPOleh {
		t.Fatalf("expected oleh, got %v", r.oleh)
	}
	sidAtom := r.oleh.FindChild(pcp.PCPHeloSessionID)
	if sidAtom == nil {
		t.Fatal("oleh has no sid child")
	}
	sid, err := sidAtom.GetID()
	if err != nil {
		t.Fatalf("get sid: %v", err)
	}
	if sid != sessionID {
		t.Errorf("oleh.sid: got %v, want %v", sid, sessionID)
	}

	if r.quit == nil || r.quit.Tag != pcp.PCPQuit {
		t.Fatalf("expected quit, got %v", r.quit)
	}
}

// TestHandlePing_BadMagic は不正なマジックバイト (12バイト未満) で接続が
// 静かに閉じられることを確認する (パニックしない)。
func TestHandlePing_BadMagic(t *testing.T) {
	client, server := net.Pipe()

	go func() {
		client.Write([]byte{0x00, 0x01})
		client.Close()
	}()

	br := bufio.NewReader(server)
	handlePing(server, br, pcp.GnuID{})
}

// TestHandlePing_InvalidHelo は pcp\n 後に helo 以外のアトムが来た場合に
// 静かに閉じられることを確認する (パニックしない)。
func TestHandlePing_InvalidHelo(t *testing.T) {
	client, server := net.Pipe()

	go func() {
		defer client.Close()
		writePCPMagic(client)
		pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown).Write(client)
	}()

	br := bufio.NewReader(server)
	handlePing(server, br, pcp.GnuID{})
}
