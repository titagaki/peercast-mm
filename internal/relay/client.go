package relay

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/pcputil"
	"github.com/titagaki/peercast-mi/internal/version"
)

const (
	dialTimeout = 10 * time.Second
	readTimeout = 60 * time.Second
)

var (
	pktTypeHead = pcp.NewID4("head")
	pktTypeData = pcp.NewID4("data")
)

const (
	bcstInterval     = 120 * time.Second
	bcstWriteTimeout = 10 * time.Second
)

// stopReason classifies why a connection ended.
type stopReason int

const (
	stopReasonError       stopReason = iota // I/O or protocol error
	stopReasonUnavailable                   // quit code 1003
	stopReasonOffAir                        // quit with other code
)

// Client connects to an upstream PeerCast node and writes the received stream
// into a local channel, reconnecting on failure.
type Client struct {
	trackerAddr  string
	channelID    pcp.GnuID
	sessionID    pcp.GnuID
	listenPort   uint16
	ch           *channel.Channel

	sourceNodes  *SourceNodeList
	ignoredNodes *IgnoredNodeCollection

	globalIP atomic.Uint32 // learned from YP oleh

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// SetGlobalIP updates the global IP address reported in BCST HOST atoms.
func (c *Client) SetGlobalIP(ip uint32) {
	c.globalIP.Store(ip)
}

// New creates a new relay Client.
func New(trackerAddr string, channelID, sessionID pcp.GnuID, listenPort uint16, ch *channel.Channel) *Client {
	return &Client{
		trackerAddr:  trackerAddr,
		channelID:    channelID,
		sessionID:    sessionID,
		listenPort:   listenPort,
		ch:           ch,
		sourceNodes:  NewSourceNodeList(),
		ignoredNodes: NewIgnoredNodeCollection(),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
}

// Run connects to the upstream node and runs the receive loop, reconnecting
// immediately on failure. It blocks until Stop is called or no connectable
// host remains.
//
// Reconnection follows PeerCastStation's algorithm:
//   - Source nodes learned from PCP HOST atoms are scored and the best
//     candidate is chosen.
//   - Hosts that return UNAVAILABLE (code 1003) or fail are temporarily
//     ignored for 3 minutes.
//   - Reconnection is immediate (no backoff); when all hosts including the
//     tracker are exhausted, the client stops (NoHost).
//   - OffAir / Error from a non-tracker host causes an ignore + immediate
//     retry on the next host. OffAir / Error from the tracker stops the client.
func (c *Client) Run() {
	defer close(c.doneCh)

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		targetAddr := selectSourceHost(c.sourceNodes.List(), c.ignoredNodes, c.trackerAddr)
		if targetAddr == "" {
			slog.Info("relay: no connectable host, stopping")
			return
		}

		reason, err := c.connectTo(targetAddr)
		if err != nil {
			if reason == stopReasonUnavailable {
				slog.Warn("relay: connection error", "addr", targetAddr, "err", err)
			} else {
				slog.Error("relay: connection error", "addr", targetAddr, "err", err)
			}
		}

		switch reason {
		case stopReasonUnavailable:
			// Host is full — ignore it and immediately try the next best.
			c.ignoredNodes.Add(targetAddr)
			continue
		case stopReasonOffAir, stopReasonError:
			// Non-tracker: ignore + retry next host (matches PeerCastStation).
			// Tracker: stop.
			if targetAddr != c.trackerAddr {
				c.ignoredNodes.Add(targetAddr)
				continue
			}
			if reason == stopReasonOffAir {
				slog.Info("relay: channel off-air, stopping", "addr", targetAddr)
			} else {
				slog.Info("relay: tracker connection failed, stopping", "addr", targetAddr)
			}
			return
		}
	}
}

// Stop signals the client to shut down and waits for it to exit.
func (c *Client) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
}

func (c *Client) connectTo(addr string) (stopReason, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return stopReasonError, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	statusCode, br, reason, err := c.handshake(conn, addr)
	if err != nil {
		return reason, err
	}

	if statusCode == 503 {
		return c.processHosts(conn, br)
	}

	// Send periodic BCST HOST atoms upstream so intermediate nodes can
	// populate their relay tree with peercast-mi's version info.
	localIP := connLocalIP(conn)
	bcstStop := make(chan struct{})
	go c.bcstHostLoop(conn, localIP, bcstStop)
	defer close(bcstStop)

	reason, err = c.processBody(conn, br)

	// Send PCP_QUIT on disconnect (best-effort, 3-second timeout).
	quitAtom := pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown)
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	quitAtom.Write(conn)
	conn.SetWriteDeadline(time.Time{})

	return reason, err
}

// handshake performs the PCP relay handshake over an established connection:
// sends the HTTP GET + helo atom, reads the HTTP response + oleh atom.
func (c *Client) handshake(conn net.Conn, addr string) (int, *bufio.Reader, stopReason, error) {
	chanIDHex := hex.EncodeToString(c.channelID[:])

	// 1. Send HTTP GET /channel/<id> with PCP upgrade header.
	// x-peercast-pos tells the upstream where to resume from, matching
	// PeerCastStation which sends Channel.ContentPosition on reconnect.
	contentPos := c.ch.ContentPosition()
	req := fmt.Sprintf("GET /channel/%s HTTP/1.0\r\nHost: %s\r\nx-peercast-pcp: 1\r\nx-peercast-pos: %d\r\n\r\n", chanIDHex, addr, contentPos)
	if _, err := io.WriteString(conn, req); err != nil {
		return 0, nil, stopReasonError, fmt.Errorf("write GET: %w", err)
	}

	// 2. Send helo atom.
	// Note: the pcp\n PCP_CONNECT magic is sent only for direct TCP connections
	// (not HTTP-upgraded /channel/ requests), so it is intentionally omitted here.
	helo := (&pcp.HeloPacket{
		Agent:     version.AgentName,
		SessionID: c.sessionID,
		Version:   version.PCPVersion,
		Port:      c.listenPort,
	}).BuildHeloAtom()
	if err := helo.Write(conn); err != nil {
		return 0, nil, stopReasonError, fmt.Errorf("write helo: %w", err)
	}

	br := bufio.NewReader(conn)

	// 3. Read HTTP response headers.
	statusCode, err := readHTTPStatus(br)
	if err != nil {
		return 0, nil, stopReasonError, fmt.Errorf("read HTTP response: %w", err)
	}
	if statusCode != 200 && statusCode != 503 {
		return statusCode, nil, stopReasonError, fmt.Errorf("upstream returned HTTP %d", statusCode)
	}

	// 4. Read oleh atom.
	oleh, err := pcp.ReadAtom(br)
	if err != nil {
		return 0, nil, stopReasonError, fmt.Errorf("read oleh: %w", err)
	}
	if oleh.Tag != pcp.PCPOleh {
		if oleh.Tag == pcp.PCPQuit {
			code, _ := oleh.GetInt()
			reason := stopReasonOffAir
			if code == pcp.PCPErrorQuit+pcp.PCPErrorUnavailable {
				reason = stopReasonUnavailable
			}
			return 0, nil, reason, fmt.Errorf("quit from upstream (code %d)", code)
		}
		return 0, nil, stopReasonError, fmt.Errorf("expected oleh, got %s", oleh.Tag)
	}

	slog.Info("relay: connected", "addr", addr, "channel", chanIDHex)
	return statusCode, br, stopReasonError, nil
}

// processBody handles the main receive loop for a 200 (connected) relay.
// It processes all atom types: PCP_CHAN, PCP_HOST, PCP_BCST, PCP_OK, PCP_QUIT.
func (c *Client) processBody(conn net.Conn, br *bufio.Reader) (stopReason, error) {
	for {
		select {
		case <-c.stopCh:
			return stopReasonOffAir, nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		atom, err := pcp.ReadAtom(br)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			select {
			case <-c.stopCh:
				return stopReasonOffAir, nil
			default:
			}
			return stopReasonError, fmt.Errorf("read atom: %w", err)
		}

		switch atom.Tag {
		case pcp.PCPChan:
			c.handleChan(atom)
		case pcp.PCPHost:
			if node, ok := parseSourceNode(atom); ok {
				c.sourceNodes.Add(node)
			}
		case pcp.PCPBcst:
			c.handleBcst(atom)
		case pcp.PCPOK:
			// No-op (matches PeerCastStation).
		case pcp.PCPQuit:
			code, _ := atom.GetInt()
			reason := stopReasonOffAir
			if code == pcp.PCPErrorQuit+pcp.PCPErrorUnavailable {
				reason = stopReasonUnavailable
			}
			return reason, fmt.Errorf("quit from upstream (code %d)", code)
		}
	}
}

// processHosts handles a 503 response: only PCP_HOST and PCP_QUIT are
// processed. No BCST HOST atoms are sent upstream.
func (c *Client) processHosts(conn net.Conn, br *bufio.Reader) (stopReason, error) {
	for {
		select {
		case <-c.stopCh:
			return stopReasonOffAir, nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(readTimeout))
		atom, err := pcp.ReadAtom(br)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			select {
			case <-c.stopCh:
				return stopReasonOffAir, nil
			default:
			}
			return stopReasonError, fmt.Errorf("read atom: %w", err)
		}

		switch atom.Tag {
		case pcp.PCPHost:
			if node, ok := parseSourceNode(atom); ok {
				c.sourceNodes.Add(node)
			}
		case pcp.PCPQuit:
			code, _ := atom.GetInt()
			reason := stopReasonOffAir
			if code == pcp.PCPErrorQuit+pcp.PCPErrorUnavailable {
				reason = stopReasonUnavailable
			}
			return reason, fmt.Errorf("quit from upstream (code %d)", code)
		}
	}
}


func (c *Client) handleChan(atom *pcp.Atom) {
	ch, err := pcp.ParseChanPacket(atom)
	if err != nil {
		return
	}
	if !ch.BroadcastID.IsEmpty() {
		c.ch.SetBroadcastID(ch.BroadcastID)
	}
	if ch.Info != nil {
		c.ch.SetInfo(channel.ChannelInfoFromPCP(*ch.Info))
	}
	if ch.Track != nil {
		c.ch.SetTrack(channel.TrackInfoFromPCP(*ch.Track))
	}
	if ch.Pkt != nil {
		c.handlePkt(ch.Pkt)
	}
}

// handleBcst processes a PCP_BCST atom by extracting child atoms
// (PCP_HOST, PCP_CHAN, etc.) and processing them locally.
func (c *Client) handleBcst(atom *pcp.Atom) {
	for _, child := range atom.Children() {
		switch child.Tag {
		case pcp.PCPHost:
			if node, ok := parseSourceNode(child); ok {
				c.sourceNodes.Add(node)
			}
		case pcp.PCPChan:
			c.handleChan(child)
		}
	}
}

func (c *Client) handlePkt(p *pcp.ChanPktData) {
	switch p.Type {
	case pktTypeHead:
		if len(p.Data) > 0 {
			c.ch.SetHeader(p.Data)
		}
	case pktTypeData:
		if len(p.Data) == 0 {
			return
		}
		c.ch.Write(p.Data, p.Pos, p.Continuation)
	}
}

// bcstHostLoop sends BCST HOST atoms upstream periodically (every 120s) or
// when the local listener/relay count changes, matching PeerCastStation's
// CheckHostInfoUpdate logic.
func (c *Client) bcstHostLoop(conn net.Conn, localIP uint32, stop <-chan struct{}) {
	uphostIP, uphostPort := connRemoteIPPort(conn)
	lastListeners := c.ch.NumListeners()
	lastRelays := c.ch.NumRelays()
	c.writeBcstHost(conn, localIP, uphostIP, uphostPort)

	t := time.NewTicker(bcstInterval)
	defer t.Stop()
	// Check for stat changes more frequently than the full interval.
	statCheck := time.NewTicker(5 * time.Second)
	defer statCheck.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			lastListeners = c.ch.NumListeners()
			lastRelays = c.ch.NumRelays()
			c.writeBcstHost(conn, localIP, uphostIP, uphostPort)
		case <-statCheck.C:
			l, r := c.ch.NumListeners(), c.ch.NumRelays()
			if l != lastListeners || r != lastRelays {
				lastListeners, lastRelays = l, r
				c.writeBcstHost(conn, localIP, uphostIP, uphostPort)
				t.Reset(bcstInterval)
			}
		}
	}
}

func (c *Client) writeBcstHost(conn net.Conn, localIP, uphostIP uint32, uphostPort uint16) {
	atom := c.buildRelayBcstAtom(localIP, uphostIP, uphostPort)
	conn.SetWriteDeadline(time.Now().Add(bcstWriteTimeout))
	atom.Write(conn)
	conn.SetWriteDeadline(time.Time{})
}

func (c *Client) buildRelayBcstAtom(localIP, uphostIP uint32, uphostPort uint16) *pcp.Atom {
	globalIP := c.globalIP.Load()
	hostAtom := pcputil.BuildHostAtom(pcputil.HostAtomParams{
		SessionID:    c.sessionID,
		LocalIP:      localIP,
		GlobalIP:     globalIP,
		ListenPort:   c.listenPort,
		ChannelID:    c.channelID,
		NumListeners: c.ch.NumListeners(),
		NumRelays:    c.ch.NumRelays(),
		Uptime:       c.ch.UptimeSeconds(),
		OldPos:       c.ch.OldestPos(),
		NewPos:       c.ch.NewestPos(),
		HasGlobalIP:  globalIP != 0,
		UphostIP:     uphostIP,
		UphostPort:   uphostPort,
		UphostHops:   1,
	})
	return pcp.NewParentAtom(pcp.PCPBcst,
		pcp.NewByteAtom(pcp.PCPBcstTTL, 11),
		pcp.NewByteAtom(pcp.PCPBcstHops, 0),
		pcp.NewIDAtom(pcp.PCPBcstFrom, c.sessionID),
		pcp.NewByteAtom(pcp.PCPBcstGroup, byte(pcp.PCPBcstGroupTrackers)),
		pcp.NewIDAtom(pcp.PCPBcstChanID, c.channelID),
		pcp.NewIntAtom(pcp.PCPBcstVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPBcstVersionVP, version.PCPVersionVP),
		pcp.NewBytesAtom(pcp.PCPBcstVersionExPrefix, []byte(version.ExPrefix)),
		pcp.NewShortAtom(pcp.PCPBcstVersionExNumber, version.ExNumber()),
		hostAtom,
	)
}

func connRemoteIPPort(conn net.Conn) (uint32, uint16) {
	tcp, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return 0, 0
	}
	v, _ := pcp.IPv4ToUint32(tcp.IP)
	return v, uint16(tcp.Port)
}

func connLocalIP(conn net.Conn) uint32 {
	tcp, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	v, _ := pcp.IPv4ToUint32(tcp.IP)
	return v
}

// readHTTPStatus reads the HTTP status line and all headers from br,
// returning the status code. Subsequent reads from br yield the PCP stream.
func readHTTPStatus(br *bufio.Reader) (int, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("read status line: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("invalid HTTP status line: %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, fmt.Errorf("invalid status code %q: %w", fields[1], err)
	}
	// Discard headers until blank line.
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return code, fmt.Errorf("read headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return code, nil
}
