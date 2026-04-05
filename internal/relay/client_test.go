package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
)

func newTestChannel() *channel.Channel {
	return channel.New(pcp.GnuID{}, pcp.GnuID{}, 0)
}

func newTestClient(addr string, ch *channel.Channel) *Client {
	return New(addr, pcp.GnuID{}, pcp.GnuID{}, 0, ch)
}

// --- readHTTPStatus ---

func TestReadHTTPStatus_OK(t *testing.T) {
	resp := "HTTP/1.0 200 OK\r\nContent-Type: application/x-peercast-pcp\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(resp))
	code, err := readHTTPStatus(br)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("got %d, want 200", code)
	}
}

func TestReadHTTPStatus_NotFound(t *testing.T) {
	resp := "HTTP/1.0 404 Not Found\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(resp))
	code, err := readHTTPStatus(br)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Fatalf("got %d, want 404", code)
	}
}

func TestReadHTTPStatus_InvalidLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("garbage\r\n\r\n"))
	_, err := readHTTPStatus(br)
	if err == nil {
		t.Fatal("expected error for invalid status line")
	}
}

func TestReadHTTPStatus_Empty(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(""))
	_, err := readHTTPStatus(br)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

// --- ParseChanInfo ---

func TestParseChanInfo(t *testing.T) {
	atom := pcp.NewParentAtom(pcp.PCPChanInfo,
		pcp.NewStringAtom(pcp.PCPChanInfoName, "Test Channel"),
		pcp.NewStringAtom(pcp.PCPChanInfoURL, "http://example.com"),
		pcp.NewStringAtom(pcp.PCPChanInfoDesc, "A test channel"),
		pcp.NewStringAtom(pcp.PCPChanInfoComment, "hello"),
		pcp.NewStringAtom(pcp.PCPChanInfoGenre, "Variety"),
		pcp.NewStringAtom(pcp.PCPChanInfoType, "FLV"),
		pcp.NewIntAtom(pcp.PCPChanInfoBitrate, 500),
	)
	ci, err := pcp.ParseChanInfo(atom)
	if err != nil {
		t.Fatalf("ParseChanInfo: %v", err)
	}
	info := channel.ChannelInfoFromPCP(ci)

	if info.Name != "Test Channel" {
		t.Errorf("Name = %q, want %q", info.Name, "Test Channel")
	}
	if info.URL != "http://example.com" {
		t.Errorf("URL = %q, want %q", info.URL, "http://example.com")
	}
	if info.Desc != "A test channel" {
		t.Errorf("Desc = %q, want %q", info.Desc, "A test channel")
	}
	if info.Comment != "hello" {
		t.Errorf("Comment = %q, want %q", info.Comment, "hello")
	}
	if info.Genre != "Variety" {
		t.Errorf("Genre = %q, want %q", info.Genre, "Variety")
	}
	if info.Type != "FLV" {
		t.Errorf("Type = %q, want %q", info.Type, "FLV")
	}
	if info.Bitrate != 500 {
		t.Errorf("Bitrate = %d, want 500", info.Bitrate)
	}
	if info.MIMEType != "video/x-flv" {
		t.Errorf("MIMEType = %q, want %q", info.MIMEType, "video/x-flv")
	}
	if info.Ext != ".flv" {
		t.Errorf("Ext = %q, want %q", info.Ext, ".flv")
	}
}

func TestParseChanInfo_UnknownType(t *testing.T) {
	atom := pcp.NewParentAtom(pcp.PCPChanInfo,
		pcp.NewStringAtom(pcp.PCPChanInfoType, "OGG"),
	)
	ci, err := pcp.ParseChanInfo(atom)
	if err != nil {
		t.Fatalf("ParseChanInfo: %v", err)
	}
	info := channel.ChannelInfoFromPCP(ci)
	if info.MIMEType != "" {
		t.Errorf("MIMEType = %q, want empty for unknown type", info.MIMEType)
	}
}

// --- ParseChanTrack ---

func TestParseChanTrack(t *testing.T) {
	atom := pcp.NewParentAtom(pcp.PCPChanTrack,
		pcp.NewStringAtom(pcp.PCPChanTrackTitle, "Song Title"),
		pcp.NewStringAtom(pcp.PCPChanTrackCreator, "Artist"),
		pcp.NewStringAtom(pcp.PCPChanTrackURL, "http://music.example.com"),
		pcp.NewStringAtom(pcp.PCPChanTrackAlbum, "Album Name"),
	)
	ct, err := pcp.ParseChanTrack(atom)
	if err != nil {
		t.Fatalf("ParseChanTrack: %v", err)
	}
	track := channel.TrackInfoFromPCP(ct)

	if track.Title != "Song Title" {
		t.Errorf("Title = %q, want %q", track.Title, "Song Title")
	}
	if track.Creator != "Artist" {
		t.Errorf("Creator = %q, want %q", track.Creator, "Artist")
	}
	if track.URL != "http://music.example.com" {
		t.Errorf("URL = %q, want %q", track.URL, "http://music.example.com")
	}
	if track.Album != "Album Name" {
		t.Errorf("Album = %q, want %q", track.Album, "Album Name")
	}
}

// --- handleChan ---

func TestHandleChan_BroadcastID(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	bcID := pcp.GnuID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanBCID, bcID),
	)
	c.handleChan(atom)

	if ch.BroadcastID() != bcID {
		t.Errorf("BroadcastID = %v, want %v", ch.BroadcastID(), bcID)
	}
}

func TestHandleChan_InfoAndTrack(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	atom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewParentAtom(pcp.PCPChanInfo,
			pcp.NewStringAtom(pcp.PCPChanInfoName, "My Channel"),
			pcp.NewStringAtom(pcp.PCPChanInfoType, "FLV"),
		),
		pcp.NewParentAtom(pcp.PCPChanTrack,
			pcp.NewStringAtom(pcp.PCPChanTrackTitle, "My Song"),
		),
	)
	c.handleChan(atom)

	if ch.Info().Name != "My Channel" {
		t.Errorf("Info.Name = %q, want %q", ch.Info().Name, "My Channel")
	}
	if ch.Track().Title != "My Song" {
		t.Errorf("Track.Title = %q, want %q", ch.Track().Title, "My Song")
	}
}

// --- handlePkt ---

func TestHandlePkt_Head(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	headerData := []byte("FLV\x01\x05")
	c.handlePkt(&pcp.ChanPktData{
		Type: pktTypeHead,
		Pos:  0,
		Data: headerData,
	})

	got, _ := ch.Header()
	if !bytes.Equal(got, headerData) {
		t.Errorf("Header = %v, want %v", got, headerData)
	}
}

func TestHandlePkt_Data(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	c.handlePkt(&pcp.ChanPktData{
		Type: pktTypeData,
		Pos:  100,
		Data: payload,
	})

	packets := ch.Since(0)
	if len(packets) != 1 {
		t.Fatalf("got %d packets, want 1", len(packets))
	}
	if !bytes.Equal(packets[0].Data, payload) {
		t.Errorf("Data = %v, want %v", packets[0].Data, payload)
	}
	if packets[0].Pos != 100 {
		t.Errorf("Pos = %d, want 100", packets[0].Pos)
	}
}

func TestHandlePkt_MissingType(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	// pkt without type → should be silently ignored
	c.handlePkt(&pcp.ChanPktData{
		Pos:  0,
		Data: []byte{1, 2, 3},
	})

	packets := ch.Since(0)
	if len(packets) != 0 {
		t.Fatalf("expected no packets, got %d", len(packets))
	}
}

// --- receiveLoop ---

func TestReceiveLoop_QuitAtom(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	// Write a quit atom into a pipe for the receive loop to read.
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		quit := pcp.NewIntAtom(pcp.PCPQuit, 1000)
		quit.Write(serverConn)
	}()

	br := bufio.NewReader(clientConn)
	_, err := c.processBody(clientConn, br)
	if err == nil {
		t.Fatal("expected error on quit")
	}
	if !strings.Contains(err.Error(), "quit from upstream") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReceiveLoop_Stop(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	done := make(chan error, 1)
	go func() {
		br := bufio.NewReader(clientConn)
		_, err := c.processBody(clientConn, br)
		done <- err
	}()

	// Let the loop start, then stop.
	time.Sleep(20 * time.Millisecond)
	c.stopOnce.Do(func() { close(c.stopCh) })
	clientConn.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error on stop, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not exit after stop")
	}
}

func TestReceiveLoop_ChanAtom(t *testing.T) {
	ch := newTestChannel()
	c := New("127.0.0.1:7144", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		// Send a chan atom with info, then a quit atom to end the loop.
		chanAtom := pcp.NewParentAtom(pcp.PCPChan,
			pcp.NewParentAtom(pcp.PCPChanInfo,
				pcp.NewStringAtom(pcp.PCPChanInfoName, "Relay Test"),
			),
		)
		chanAtom.Write(serverConn)

		quit := pcp.NewIntAtom(pcp.PCPQuit, 0)
		quit.Write(serverConn)
	}()

	br := bufio.NewReader(clientConn)
	_, _ = c.processBody(clientConn, br)

	if ch.Info().Name != "Relay Test" {
		t.Errorf("Info.Name = %q, want %q", ch.Info().Name, "Relay Test")
	}
}

// --- connect (integration with fake upstream) ---

func TestConnect_FullHandshake(t *testing.T) {
	channelID := pcp.GnuID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	sessionID := pcp.GnuID{0xAA}
	ch := newTestChannel()

	// Start a fake upstream listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- fakeUpstream(t, ln, channelID)
	}()

	c := New(ln.Addr().String(), channelID, sessionID, 0, ch)
	_, err = c.connectTo(ln.Addr().String())
	// connectTo returns a "quit" error because the fake upstream sends a quit atom
	// to end the session — this is expected.
	if err != nil && !strings.Contains(err.Error(), "quit from upstream") {
		t.Fatalf("connectTo: %v", err)
	}

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("fake upstream error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake upstream timed out")
	}

	// Verify the channel got updated by the fake upstream.
	if ch.Info().Name != "Upstream Channel" {
		t.Errorf("Info.Name = %q, want %q", ch.Info().Name, "Upstream Channel")
	}
}

// fakeUpstream simulates an upstream PeerCast node. It accepts one connection,
// validates the client handshake, sends an oleh + a chan atom + a quit atom.
func fakeUpstream(t *testing.T, ln net.Listener, expectedChanID pcp.GnuID) error {
	t.Helper()

	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	br := bufio.NewReader(conn)

	// 1. Read HTTP GET request.
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read request line: %w", err)
	}
	if !strings.HasPrefix(reqLine, "GET /channel/") {
		return fmt.Errorf("unexpected request: %q", reqLine)
	}
	// Skip remaining headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 2. Read helo atom (no pcp\n magic for /channel/ HTTP-upgraded connections).
	helo, err := pcp.ReadAtom(br)
	if err != nil {
		return fmt.Errorf("read helo: %w", err)
	}
	if helo.Tag != pcp.PCPHelo {
		return fmt.Errorf("expected helo, got %s", helo.Tag)
	}

	// 4. Send HTTP response.
	if _, err := io.WriteString(conn, "HTTP/1.0 200 OK\r\n\r\n"); err != nil {
		return fmt.Errorf("write HTTP response: %w", err)
	}

	// 5. Send oleh atom.
	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewStringAtom(pcp.PCPHeloAgent, "FakeUpstream/1.0"),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, pcp.GnuID{0xFF}),
	)
	if err := oleh.Write(conn); err != nil {
		return fmt.Errorf("write oleh: %w", err)
	}

	// 6. Send a chan atom with info.
	chanAtom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewParentAtom(pcp.PCPChanInfo,
			pcp.NewStringAtom(pcp.PCPChanInfoName, "Upstream Channel"),
			pcp.NewStringAtom(pcp.PCPChanInfoType, "FLV"),
		),
	)
	if err := chanAtom.Write(conn); err != nil {
		return fmt.Errorf("write chan: %w", err)
	}

	// 7. Send quit to end the session.
	quit := pcp.NewIntAtom(pcp.PCPQuit, 0)
	if err := quit.Write(conn); err != nil {
		return fmt.Errorf("write quit: %w", err)
	}

	return nil
}

// --- Run / Stop lifecycle ---

func TestRunStop_TrackerFail(t *testing.T) {
	ch := newTestChannel()
	// Use an invalid address so connect to tracker fails immediately.
	// With no backoff, Run should stop after the tracker fails (no other hosts).
	c := New("127.0.0.1:1", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	done := make(chan struct{})
	go func() {
		c.Run()
		close(done)
	}()

	select {
	case <-done:
		// OK �� Run exited because tracker connection failed.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after tracker failure")
	}
}

func TestRunStop_NonTrackerIgnored(t *testing.T) {
	ch := newTestChannel()
	// Start a listener that accepts and immediately closes, so connectTo
	// fails quickly. The node is a non-tracker, so it should be ignored
	// and Run should eventually stop when no hosts remain.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// Tracker is unreachable; one non-tracker source node closes immediately.
	// Flow: source node → error → ignored → tracker → refused → stop.
	c := New("127.0.0.1:1", pcp.GnuID{}, pcp.GnuID{}, 0, ch)
	c.sourceNodes.Add(SourceNode{
		SessionID:   pcp.GnuID{0x01},
		GlobalAddr:  ln.Addr().String(),
		IsReceiving: true,
	})

	done := make(chan struct{})
	go func() {
		c.Run()
		close(done)
	}()

	select {
	case <-done:
		// OK — Run exited after exhausting all hosts.
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after all hosts exhausted")
	}
}

func TestStopIdempotent(t *testing.T) {
	ch := newTestChannel()
	// Use an unreachable tracker so Run exits immediately (connection refused).
	c := New("127.0.0.1:1", pcp.GnuID{}, pcp.GnuID{}, 0, ch)

	done := make(chan struct{})
	go func() {
		c.Run()
		close(done)
	}()

	<-done

	// Multiple Stop calls after Run exited should not panic.
	c.Stop()
	c.Stop()
}
