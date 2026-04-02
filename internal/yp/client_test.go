package yp

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/id"
	"github.com/titagaki/peercast-mi/internal/version"
)

// fakeYP simulates a YP server on one side of a net.Pipe.
// It reads the magic atom and helo, responds with oleh + ok,
// then provides channels for the test to read bcst atoms and send stop.
type fakeYP struct {
	conn     net.Conn
	pcpConn  *pcp.Conn
	heloAtom *pcp.Atom
}

// acceptMagicAndHelo reads the magic bytes (from pcp.NewConn) and the helo atom.
func newFakeYP(conn net.Conn) (*fakeYP, error) {
	// pcp.NewConn writes 8-byte magic; read it manually.
	magic := make([]byte, 8)
	if _, err := conn.Read(magic); err != nil {
		return nil, err
	}

	helo, err := pcp.ReadAtom(conn)
	if err != nil {
		return nil, err
	}

	return &fakeYP{conn: conn, heloAtom: helo}, nil
}

func (f *fakeYP) sendOlehAndOK(remoteIP uint32) error {
	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewIntAtom(pcp.PCPHeloRemoteIP, remoteIP),
	)
	if err := oleh.Write(f.conn); err != nil {
		return err
	}
	return pcp.NewEmptyAtom(pcp.PCPOK).Write(f.conn)
}

func (f *fakeYP) sendOlehRootAndOK(remoteIP uint32, updateInterval uint32) error {
	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewIntAtom(pcp.PCPHeloRemoteIP, remoteIP),
	)
	if err := oleh.Write(f.conn); err != nil {
		return err
	}
	root := pcp.NewParentAtom(pcp.PCPRoot,
		pcp.NewIntAtom(pcp.PCPRootUpdInt, updateInterval),
	)
	if err := root.Write(f.conn); err != nil {
		return err
	}
	return pcp.NewEmptyAtom(pcp.PCPOK).Write(f.conn)
}

func (f *fakeYP) readAtom() (*pcp.Atom, error) {
	return pcp.ReadAtom(f.conn)
}

func (f *fakeYP) close() {
	f.conn.Close()
}

// helpers ---------------------------------------------------------------

func setupClient(t *testing.T, serverConn net.Conn) (*Client, pcp.GnuID, pcp.GnuID) {
	t.Helper()
	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)
	// Use the server-side address; client dials our pipe.
	c := New("unused", sid, bcid, mgr, 7144)
	return c, sid, bcid
}

// dialPipe creates a pair of connected pipes and a pcp.Conn from the client side.
// Returns (client pcp.Conn, server net.Conn).
func dialPipe(t *testing.T) (*pcp.Conn, net.Conn) {
	t.Helper()
	clientRaw, serverRaw := net.Pipe()
	// pcp.NewConn sends magic bytes immediately.
	var pcpConn *pcp.Conn
	var connErr error
	done := make(chan struct{})
	go func() {
		pcpConn, connErr = pcp.NewConn(clientRaw)
		close(done)
	}()
	// Server needs to be ready to receive magic bytes.
	magic := make([]byte, 8)
	if _, err := serverRaw.Read(magic); err != nil {
		t.Fatal(err)
	}
	<-done
	if connErr != nil {
		t.Fatal(connErr)
	}
	return pcpConn, serverRaw
}

// -----------------------------------------------------------------------
// New
// -----------------------------------------------------------------------

func TestNew_DefaultPort(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	c := New("localhost:7144", id.NewRandom(), id.NewRandom(), mgr, 0)
	if c.listenPort != defaultPCPPort {
		t.Errorf("listenPort: got %d, want %d", c.listenPort, defaultPCPPort)
	}
}

func TestNew_CustomPort(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	c := New("localhost:7144", id.NewRandom(), id.NewRandom(), mgr, 8144)
	if c.listenPort != 8144 {
		t.Errorf("listenPort: got %d, want 8144", c.listenPort)
	}
}

// -----------------------------------------------------------------------
// buildHelo
// -----------------------------------------------------------------------

func TestBuildHelo(t *testing.T) {
	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)
	c := New("localhost:7144", sid, bcid, mgr, 9000)

	helo := c.buildHelo()

	if helo.Tag != pcp.PCPHelo {
		t.Fatalf("tag: got %v, want helo", helo.Tag)
	}

	// Check agent
	agnt := helo.FindChild(pcp.PCPHeloAgent)
	if agnt == nil {
		t.Fatal("missing agnt")
	}
	if got := agnt.GetString(); got != version.AgentName {
		t.Errorf("agent: got %q, want %q", got, version.AgentName)
	}

	// Check version
	ver := helo.FindChild(pcp.PCPHeloVersion)
	if ver == nil {
		t.Fatal("missing ver")
	}
	if v, err := ver.GetInt(); err != nil || v != version.PCPVersion {
		t.Errorf("version: got %d, want %d", v, version.PCPVersion)
	}

	// Check session ID
	sidAtom := helo.FindChild(pcp.PCPHeloSessionID)
	if sidAtom == nil {
		t.Fatal("missing sid")
	}
	if gotID, err := sidAtom.GetID(); err != nil || gotID != sid {
		t.Error("session ID mismatch")
	}

	// Check port
	portAtom := helo.FindChild(pcp.PCPHeloPort)
	if portAtom == nil {
		t.Fatal("missing port")
	}
	if v, err := portAtom.GetShort(); err != nil || v != 9000 {
		t.Errorf("port: got %d, want 9000", v)
	}

	// Check broadcast ID
	bcidAtom := helo.FindChild(pcp.PCPHeloBCID)
	if bcidAtom == nil {
		t.Fatal("missing bcid")
	}
	if gotID, err := bcidAtom.GetID(); err != nil || gotID != bcid {
		t.Error("broadcast ID mismatch")
	}
}

// -----------------------------------------------------------------------
// buildBcst
// -----------------------------------------------------------------------

func TestBuildBcst(t *testing.T) {
	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)
	c := New("localhost:7144", sid, bcid, mgr, 7144)
	c.globalIP = 0xC0A80001 // 192.168.0.1

	const key = "sk_testkey"
	mgr.IssueStreamKey("test-account", key)
	info := channel.ChannelInfo{
		Name:    "TestCH",
		Genre:   "game",
		URL:     "http://example.com",
		Desc:    "description",
		Comment: "comment",
		Type:    "FLV",
		Bitrate: 500,
	}
	track := channel.TrackInfo{
		Title:   "Track1",
		Creator: "DJ",
		URL:     "http://track.example.com",
		Album:   "Album1",
	}
	ch, err := mgr.Broadcast(key, info, track)
	if err != nil {
		t.Fatal(err)
	}

	bcst := c.buildBcst(ch)

	if bcst.Tag != pcp.PCPBcst {
		t.Fatalf("tag: got %v, want bcst", bcst.Tag)
	}

	// TTL
	ttlAtom := bcst.FindChild(pcp.PCPBcstTTL)
	if ttlAtom == nil {
		t.Fatal("missing ttl")
	}
	if v, err := ttlAtom.GetByte(); err != nil || v != bcstTTL {
		t.Errorf("ttl: got %d, want %d", v, bcstTTL)
	}

	// Hops
	hopsAtom := bcst.FindChild(pcp.PCPBcstHops)
	if hopsAtom == nil {
		t.Fatal("missing hops")
	}
	if v, err := hopsAtom.GetByte(); err != nil || v != 0 {
		t.Errorf("hops: got %d, want 0", v)
	}

	// From
	fromAtom := bcst.FindChild(pcp.PCPBcstFrom)
	if fromAtom == nil {
		t.Fatal("missing from")
	}
	if gotID, err := fromAtom.GetID(); err != nil || gotID != sid {
		t.Error("from should be sessionID")
	}

	// Group
	grpAtom := bcst.FindChild(pcp.PCPBcstGroup)
	if grpAtom == nil {
		t.Fatal("missing grp")
	}
	if v, err := grpAtom.GetByte(); err != nil || v != byte(pcp.PCPBcstGroupRoot) {
		t.Errorf("grp: got %d, want %d", v, pcp.PCPBcstGroupRoot)
	}

	// Chan
	chanAtom := bcst.FindChild(pcp.PCPChan)
	if chanAtom == nil {
		t.Fatal("missing chan")
	}
	chanIDAtom := chanAtom.FindChild(pcp.PCPChanID)
	if chanIDAtom == nil {
		t.Fatal("missing chan.id")
	}
	if gotID, err := chanIDAtom.GetID(); err != nil || gotID != ch.ID {
		t.Error("chan.id mismatch")
	}

	// Chan info
	chanInfo := chanAtom.FindChild(pcp.PCPChanInfo)
	if chanInfo == nil {
		t.Fatal("missing chan.info")
	}
	nameAtom := chanInfo.FindChild(pcp.PCPChanInfoName)
	if nameAtom == nil || nameAtom.GetString() != "TestCH" {
		t.Error("chan.info.name mismatch")
	}
	typeAtom := chanInfo.FindChild(pcp.PCPChanInfoType)
	if typeAtom == nil || typeAtom.GetString() != "FLV" {
		t.Error("chan.info.type mismatch")
	}

	// Chan track
	chanTrack := chanAtom.FindChild(pcp.PCPChanTrack)
	if chanTrack == nil {
		t.Fatal("missing chan.trck")
	}
	titleAtom := chanTrack.FindChild(pcp.PCPChanTrackTitle)
	if titleAtom == nil || titleAtom.GetString() != "Track1" {
		t.Error("chan.trck.title mismatch")
	}

	// Host
	hostAtom := bcst.FindChild(pcp.PCPHost)
	if hostAtom == nil {
		t.Fatal("missing host")
	}
	hostIDAtom := hostAtom.FindChild(pcp.PCPHostID)
	if hostIDAtom == nil {
		t.Fatal("missing host.id")
	}
	if gotID, err := hostIDAtom.GetID(); err != nil || gotID != sid {
		t.Error("host.id should be sessionID")
	}
	hostIPAtom := hostAtom.FindChild(pcp.PCPHostIP)
	if hostIPAtom == nil {
		t.Fatal("missing host.ip")
	}
	if v, err := hostIPAtom.GetInt(); err != nil || v != 0xC0A80001 {
		t.Errorf("host.ip: got 0x%08X, want 0xC0A80001", v)
	}
	hostPortAtom := hostAtom.FindChild(pcp.PCPHostPort)
	if hostPortAtom == nil {
		t.Fatal("missing host.port")
	}
	if v, err := hostPortAtom.GetShort(); err != nil || v != 7144 {
		t.Errorf("host.port: got %d, want 7144", v)
	}
}

// -----------------------------------------------------------------------
// handleOleh
// -----------------------------------------------------------------------

func TestHandleOleh(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	c := New("localhost:7144", id.NewRandom(), id.NewRandom(), mgr, 7144)

	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x0A000001),
	)
	c.handleOleh(oleh)

	if c.globalIP != 0x0A000001 {
		t.Errorf("globalIP: got 0x%08X, want 0x0A000001", c.globalIP)
	}
}

func TestHandleOleh_NoRIP(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	c := New("localhost:7144", id.NewRandom(), id.NewRandom(), mgr, 7144)

	oleh := pcp.NewParentAtom(pcp.PCPOleh,
		pcp.NewStringAtom(pcp.PCPHeloAgent, "test"),
	)
	c.handleOleh(oleh)
	// globalIP remains 0
	if c.globalIP != 0 {
		t.Errorf("globalIP should remain 0, got 0x%08X", c.globalIP)
	}
}

// -----------------------------------------------------------------------
// parseRoot
// -----------------------------------------------------------------------

func TestParseRoot_Interval(t *testing.T) {
	root := pcp.NewParentAtom(pcp.PCPRoot,
		pcp.NewIntAtom(pcp.PCPRootUpdInt, 60),
	)
	interval, immediate := parseRoot(root)
	if interval != 60 {
		t.Errorf("interval: got %d, want 60", interval)
	}
	if immediate {
		t.Error("immediate should be false")
	}
}

func TestParseRoot_Immediate(t *testing.T) {
	root := pcp.NewParentAtom(pcp.PCPRoot,
		pcp.NewIntAtom(pcp.PCPRootUpdInt, 30),
		pcp.NewEmptyAtom(pcp.PCPRootUpdate),
	)
	interval, immediate := parseRoot(root)
	if interval != 30 {
		t.Errorf("interval: got %d, want 30", interval)
	}
	if !immediate {
		t.Error("immediate should be true")
	}
}

func TestParseRoot_NoFields(t *testing.T) {
	root := pcp.NewParentAtom(pcp.PCPRoot)
	interval, immediate := parseRoot(root)
	if interval != 0 {
		t.Errorf("interval: got %d, want 0", interval)
	}
	if immediate {
		t.Error("immediate should be false")
	}
}

// -----------------------------------------------------------------------
// ipToString
// -----------------------------------------------------------------------

func TestIPToString(t *testing.T) {
	tests := []struct {
		ip   uint32
		want string
	}{
		{0xC0A80001, "192.168.0.1"},
		{0x0A000001, "10.0.0.1"},
		{0x7F000001, "127.0.0.1"},
		{0x00000000, "0.0.0.0"},
	}
	for _, tt := range tests {
		if got := ipToString(tt.ip); got != tt.want {
			t.Errorf("ipToString(0x%08X): got %q, want %q", tt.ip, got, tt.want)
		}
	}
}

// -----------------------------------------------------------------------
// Bump
// -----------------------------------------------------------------------

func TestBump_NonBlocking(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	c := New("localhost:7144", id.NewRandom(), id.NewRandom(), mgr, 7144)

	// Bump should not block even if nobody is reading bumpCh
	c.Bump()
	c.Bump() // second call should not block (buffered channel, default branch)
}

// -----------------------------------------------------------------------
// run() — full handshake + bcst + stop integration test
// -----------------------------------------------------------------------

func TestRun_HandshakeAndBcst(t *testing.T) {
	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)

	// Create a channel so bcst has something to send.
	const key = "sk_testkey"
	mgr.IssueStreamKey("test-account", key)
	info := channel.ChannelInfo{Name: "IntegrationCH", Genre: "test", Type: "FLV", Bitrate: 300}
	ch, err := mgr.Broadcast(key, info, channel.TrackInfo{Title: "Song"})
	if err != nil {
		t.Fatal(err)
	}

	c := &Client{
		addr:        "pipe",
		sessionID:   sid,
		broadcastID: bcid,
		mgr:         mgr,
		listenPort:  7144,
		stopCh:      make(chan struct{}),
		bumpCh:      make(chan struct{}, 1),
	}

	// Test the handshake + bcst flow manually using pipes.
	clientConn, serverConn := net.Pipe()

	var serverWg sync.WaitGroup

	// Server goroutine (fake YP).
	var receivedBcst *pcp.Atom
	var receivedQuit *pcp.Atom
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		fyp, fypErr := newFakeYP(serverConn)
		if fypErr != nil {
			t.Logf("fakeYP init error: %v", fypErr)
			return
		}

		// Verify helo
		if fyp.heloAtom.Tag != pcp.PCPHelo {
			t.Errorf("expected helo, got %v", fyp.heloAtom.Tag)
		}

		// Send oleh + ok
		if sendErr := fyp.sendOlehAndOK(0xC0A80001); sendErr != nil {
			t.Logf("sendOlehAndOK error: %v", sendErr)
			return
		}

		// Read bcst
		atom, readErr := fyp.readAtom()
		if readErr != nil {
			t.Logf("read bcst error: %v", readErr)
			return
		}
		receivedBcst = atom

		// Read quit
		atom2, readErr2 := fyp.readAtom()
		if readErr2 != nil {
			t.Logf("read quit error: %v", readErr2)
			return
		}
		receivedQuit = atom2
	}()

	// Client goroutine: pcp.NewConn + run().
	// We can't use c.run() directly because it calls pcp.Dial.
	// Instead, simulate what run() does with a raw connection.
	pcpConn, connErr := pcp.NewConn(clientConn)
	if connErr != nil {
		t.Fatal(connErr)
	}

	// Send helo.
	if err := pcpConn.WriteAtom(c.buildHelo()); err != nil {
		t.Fatal(err)
	}

	// Read oleh.
	olehAtom, err := pcpConn.ReadAtom()
	if err != nil {
		t.Fatal(err)
	}
	if olehAtom.Tag != pcp.PCPOleh {
		t.Fatalf("expected oleh, got %v", olehAtom.Tag)
	}
	c.handleOleh(olehAtom)

	// Read ok.
	okAtom, err := pcpConn.ReadAtom()
	if err != nil {
		t.Fatal(err)
	}
	if okAtom.Tag != pcp.PCPOK {
		t.Fatalf("expected ok, got %v", okAtom.Tag)
	}

	// Send bcst.
	bcstAtom := c.buildBcst(ch)
	if err := pcpConn.WriteAtom(bcstAtom); err != nil {
		t.Fatal(err)
	}

	// Send quit.
	quitAtom := pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown)
	if err := pcpConn.WriteAtom(quitAtom); err != nil {
		t.Fatal(err)
	}

	pcpConn.Close()
	serverWg.Wait()

	// Verify what the server received.
	if receivedBcst == nil {
		t.Fatal("server should have received bcst")
	}
	if receivedBcst.Tag != pcp.PCPBcst {
		t.Errorf("expected bcst, got %v", receivedBcst.Tag)
	}
	// Verify bcst contains the channel ID.
	cidAtom := receivedBcst.FindChild(pcp.PCPBcstChanID)
	if cidAtom == nil {
		t.Fatal("bcst missing channel ID")
	}
	if gotID, err := cidAtom.GetID(); err != nil || gotID != ch.ID {
		t.Error("bcst channel ID mismatch")
	}

	if receivedQuit == nil {
		t.Fatal("server should have received quit")
	}
	if receivedQuit.Tag != pcp.PCPQuit {
		t.Errorf("expected quit, got %v", receivedQuit.Tag)
	}

	// Verify globalIP was set from oleh.
	if c.globalIP != 0xC0A80001 {
		t.Errorf("globalIP: got 0x%08X, want 0xC0A80001", c.globalIP)
	}
}

// -----------------------------------------------------------------------
// run() via net.Pipe — test the actual run() method
// -----------------------------------------------------------------------

// testableClient creates a Client that we can test with run() by using a pipe.
// Since run() calls pcp.Dial internally, we test run's internal logic by
// calling the unexported handshake/bcst steps individually.
// For a true integration test, we'd need to start a TCP listener.

func TestRun_FullIntegration(t *testing.T) {
	// Start a real TCP listener to act as YP.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)

	const key = "sk_testkey"
	mgr.IssueStreamKey("test-account", key)
	info := channel.ChannelInfo{Name: "FullTest", Genre: "test", Type: "FLV", Bitrate: 128}
	ch, err := mgr.Broadcast(key, info, channel.TrackInfo{})
	if err != nil {
		t.Fatal(err)
	}

	c := New(ln.Addr().String(), sid, bcid, mgr, 8000)

	// Server goroutine.
	var serverWg sync.WaitGroup
	var receivedHelo *pcp.Atom
	var receivedBcsts []*pcp.Atom
	var receivedQuit *pcp.Atom

	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read PCP magic (8 bytes).
		magic := make([]byte, 8)
		if _, err := conn.Read(magic); err != nil {
			return
		}

		// Read helo.
		helo, err := pcp.ReadAtom(conn)
		if err != nil {
			return
		}
		receivedHelo = helo

		// Verify the custom port is in helo.
		portAtom := helo.FindChild(pcp.PCPHeloPort)
		if portAtom != nil {
			if v, err := portAtom.GetShort(); err == nil && v != 8000 {
				t.Errorf("helo port: got %d, want 8000", v)
			}
		}

		// Send oleh + ok.
		oleh := pcp.NewParentAtom(pcp.PCPOleh,
			pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x7F000001),
		)
		oleh.Write(conn)
		pcp.NewEmptyAtom(pcp.PCPOK).Write(conn)

		// Read initial bcst.
		for {
			atom, err := pcp.ReadAtom(conn)
			if err != nil {
				return
			}
			if atom.Tag == pcp.PCPQuit {
				receivedQuit = atom
				return
			}
			if atom.Tag == pcp.PCPBcst {
				receivedBcsts = append(receivedBcsts, atom)
			}
		}
	}()

	// Client: run() in a goroutine, stop after a short delay.
	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		c.run()
	}()

	// Give the handshake + initial bcst time to complete.
	time.Sleep(200 * time.Millisecond)

	// Stop the client → should send quit.
	c.Stop()
	runWg.Wait()
	serverWg.Wait()

	// Verify.
	if receivedHelo == nil {
		t.Fatal("server should have received helo")
	}
	if receivedHelo.Tag != pcp.PCPHelo {
		t.Errorf("expected helo tag, got %v", receivedHelo.Tag)
	}

	if len(receivedBcsts) == 0 {
		t.Fatal("server should have received at least one bcst")
	}

	// Verify bcst contains correct channel.
	bcstCID := receivedBcsts[0].FindChild(pcp.PCPBcstChanID)
	if bcstCID == nil {
		t.Fatal("bcst missing channel ID")
	}
	if gotID, err := bcstCID.GetID(); err != nil || gotID != ch.ID {
		t.Error("bcst channel ID mismatch")
	}

	if receivedQuit == nil {
		t.Fatal("server should have received quit")
	}

	// Verify globalIP was learned.
	if c.globalIP != 0x7F000001 {
		t.Errorf("globalIP: got 0x%08X, want 0x7F000001", c.globalIP)
	}
}

// -----------------------------------------------------------------------
// run() — backoff reset
// -----------------------------------------------------------------------

func TestRun_BackoffResetsOnSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)

	// Register a channel so sendAllBcst actually writes an atom.
	const key = "sk_testkey"
	mgr.IssueStreamKey("test-account", key)
	_, err = mgr.Broadcast(key, channel.ChannelInfo{Name: "test"}, channel.TrackInfo{})
	if err != nil {
		t.Fatal(err)
	}

	c := New(ln.Addr().String(), sid, bcid, mgr, 7144)

	// Server: accept, complete handshake, then close immediately.
	// The client's initial sendAllBcst will write to a closed connection
	// and return an error, causing run() to return (true, err).
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		magic := make([]byte, 8)
		conn.Read(magic)
		pcp.ReadAtom(conn) // helo
		oleh := pcp.NewParentAtom(pcp.PCPOleh,
			pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x7F000001),
		)
		oleh.Write(conn)
		pcp.NewEmptyAtom(pcp.PCPOK).Write(conn)
		// Close immediately — don't read bcst.
		// TCP close may be delayed, so read the initial bcst first
		// before closing to ensure the close happens while client
		// is in the select loop.
		pcp.ReadAtom(conn) // initial bcst
	}()

	// run() in a goroutine.
	type result struct {
		connected bool
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		connected, err := c.run()
		resultCh <- result{connected, err}
	}()

	// Give the handshake time to complete, then bump to trigger a write
	// on the now-closed connection.
	time.Sleep(200 * time.Millisecond)
	c.Bump()

	select {
	case r := <-resultCh:
		if !r.connected {
			t.Error("run should report connected=true after successful handshake")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return in time")
	}
}

// -----------------------------------------------------------------------
// run() — no channels → empty bcst
// -----------------------------------------------------------------------

func TestRun_NoChannels(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid) // no channels

	c := New(ln.Addr().String(), sid, bcid, mgr, 7144)

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		magic := make([]byte, 8)
		conn.Read(magic)
		pcp.ReadAtom(conn) // helo
		oleh := pcp.NewParentAtom(pcp.PCPOleh,
			pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x7F000001),
		)
		oleh.Write(conn)
		pcp.NewEmptyAtom(pcp.PCPOK).Write(conn)

		// Read quit (no bcst expected for zero channels).
		for {
			atom, err := pcp.ReadAtom(conn)
			if err != nil {
				return
			}
			if atom.Tag == pcp.PCPQuit {
				return
			}
		}
	}()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		c.run()
	}()

	time.Sleep(100 * time.Millisecond)
	c.Stop()
	runWg.Wait()
	serverWg.Wait()
	// If we get here without hanging, the test passes.
}

// -----------------------------------------------------------------------
// run() — Bump triggers immediate bcst
// -----------------------------------------------------------------------

func TestRun_BumpSendsBcst(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)

	const key = "sk_testkey"
	mgr.IssueStreamKey("test-account", key)
	info := channel.ChannelInfo{Name: "BumpTest", Genre: "test", Type: "FLV", Bitrate: 64}
	_, err = mgr.Broadcast(key, info, channel.TrackInfo{})
	if err != nil {
		t.Fatal(err)
	}

	c := New(ln.Addr().String(), sid, bcid, mgr, 7144)

	var bcstCount int
	var mu sync.Mutex
	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		magic := make([]byte, 8)
		conn.Read(magic)
		pcp.ReadAtom(conn)
		oleh := pcp.NewParentAtom(pcp.PCPOleh,
			pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x7F000001),
		)
		oleh.Write(conn)
		pcp.NewEmptyAtom(pcp.PCPOK).Write(conn)

		for {
			atom, err := pcp.ReadAtom(conn)
			if err != nil {
				return
			}
			if atom.Tag == pcp.PCPBcst {
				mu.Lock()
				bcstCount++
				mu.Unlock()
			}
			if atom.Tag == pcp.PCPQuit {
				return
			}
		}
	}()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		c.run()
	}()

	// Wait for initial bcst, then bump.
	time.Sleep(100 * time.Millisecond)
	c.Bump()
	time.Sleep(100 * time.Millisecond)

	c.Stop()
	runWg.Wait()
	serverWg.Wait()

	mu.Lock()
	count := bcstCount
	mu.Unlock()
	// Should have at least 2: initial + bump
	if count < 2 {
		t.Errorf("expected at least 2 bcst, got %d", count)
	}
}

// -----------------------------------------------------------------------
// run() — handshake quit from YP
// -----------------------------------------------------------------------

func TestRun_HandshakeQuit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)
	c := New(ln.Addr().String(), sid, bcid, mgr, 7144)

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		magic := make([]byte, 8)
		conn.Read(magic)
		pcp.ReadAtom(conn) // helo
		// Send quit instead of oleh + ok.
		pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit).Write(conn)
	}()

	connected, err := c.run()
	serverWg.Wait()

	if connected {
		t.Error("should not be connected after handshake quit")
	}
	if err == nil {
		t.Error("expected error for handshake quit")
	}
}

// -----------------------------------------------------------------------
// run() — dial failure
// -----------------------------------------------------------------------

func TestRun_DialFailure(t *testing.T) {
	mgr := channel.NewManager(id.NewRandom())
	// Use a port that is not listening.
	c := New("127.0.0.1:1", id.NewRandom(), id.NewRandom(), mgr, 7144)

	connected, err := c.run()
	if connected {
		t.Error("should not be connected on dial failure")
	}
	if err == nil {
		t.Error("expected error for dial failure")
	}
}

// -----------------------------------------------------------------------
// run() — root with custom interval
// -----------------------------------------------------------------------

func TestRun_RootInterval(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sid := id.NewRandom()
	bcid := id.NewRandom()
	mgr := channel.NewManager(bcid)
	c := New(ln.Addr().String(), sid, bcid, mgr, 7144)

	var serverWg sync.WaitGroup
	serverWg.Add(1)
	go func() {
		defer serverWg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		magic := make([]byte, 8)
		conn.Read(magic)
		pcp.ReadAtom(conn) // helo

		// Send oleh + root with custom interval + ok.
		oleh := pcp.NewParentAtom(pcp.PCPOleh,
			pcp.NewIntAtom(pcp.PCPHeloRemoteIP, 0x7F000001),
		)
		oleh.Write(conn)
		root := pcp.NewParentAtom(pcp.PCPRoot,
			pcp.NewIntAtom(pcp.PCPRootUpdInt, 60),
		)
		root.Write(conn)
		pcp.NewEmptyAtom(pcp.PCPOK).Write(conn)

		// Read atoms until quit.
		for {
			atom, err := pcp.ReadAtom(conn)
			if err != nil {
				return
			}
			if atom.Tag == pcp.PCPQuit {
				return
			}
		}
	}()

	var runWg sync.WaitGroup
	runWg.Add(1)
	go func() {
		defer runWg.Done()
		c.run()
	}()

	time.Sleep(100 * time.Millisecond)
	c.Stop()
	runWg.Wait()
	serverWg.Wait()
	// Test passes if run() accepted the root interval without issues.
}
