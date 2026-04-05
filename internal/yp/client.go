package yp

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/pcputil"
	"github.com/titagaki/peercast-mi/internal/version"
)

const (
	bcstTTL         = 11
	defaultPCPPort  = 7144
	retryInitial    = 5 * time.Second
	retryMax        = 120 * time.Second
	defaultInterval = 120 * time.Second
)

// ChannelLister provides a list of active channels for YP announcement.
type ChannelLister interface {
	List() []*channel.Channel
}

// Client maintains a PCP COUT connection to a YP (root server).
// It announces all channels currently active in the manager.
type Client struct {
	addr         string
	sessionID    pcp.GnuID
	broadcastID  pcp.GnuID
	mgr          ChannelLister
	listenPort   uint16
	maxRelays    int // per-channel 制限 (0 = unlimited)
	maxListeners int // per-channel 制限 (0 = unlimited)

	globalIP uint32 // learned from oleh.rip
	localIP  uint32 // local address of YP connection

	// OnGlobalIP is called with the global IP address learned from the YP oleh.
	// May be nil.
	OnGlobalIP func(uint32)

	stopCh   chan struct{}
	bumpCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// New creates a new YPClient.
func New(addr string, sessionID, broadcastID pcp.GnuID, mgr ChannelLister, listenPort, maxRelays, maxListeners int) *Client {
	if listenPort <= 0 {
		listenPort = defaultPCPPort
	}
	return &Client{
		addr:         addr,
		sessionID:    sessionID,
		broadcastID:  broadcastID,
		mgr:          mgr,
		listenPort:   uint16(listenPort),
		maxRelays:    maxRelays,
		maxListeners: maxListeners,
		stopCh:       make(chan struct{}),
		bumpCh:       make(chan struct{}, 1),
		doneCh:       make(chan struct{}),
	}
}

// Run connects to the YP and runs the bcst loop, reconnecting on failure.
// Before globalIP is known, it connects as soon as any channel (broadcasting
// or relay) exists, so relay-only nodes can learn their global IP from the
// YP oleh response. Once globalIP is known, it only connects when there is
// at least one broadcasting channel to announce — relay-only nodes have
// nothing to send and would be closed by the YP's read timeout.
func (c *Client) Run() {
	defer close(c.doneCh)
	delay := retryInitial
	for {
		if !c.waitForChannel() {
			return
		}
		connected, err := c.run()
		if err != nil {
			slog.Error("yp: connection error", "addr", c.addr, "err", err)
		}
		if connected {
			delay = retryInitial
		}

		select {
		case <-c.stopCh:
			return
		case <-time.After(delay):
		}

		delay *= 2
		if delay > retryMax {
			delay = retryMax
		}
	}
}

// waitForChannel blocks until shouldConnect returns true, or until the client
// is stopped.
func (c *Client) waitForChannel() bool {
	for {
		if c.shouldConnect() {
			return true
		}
		select {
		case <-c.stopCh:
			return false
		case <-c.bumpCh:
			// re-check immediately
		case <-time.After(time.Second):
			// poll
		}
	}
}

// shouldConnect decides whether the client has a reason to (re)connect to
// the YP right now. See Run's comment for the rationale.
func (c *Client) shouldConnect() bool {
	channels := c.mgr.List()
	if len(channels) == 0 {
		return false
	}
	if c.globalIP == 0 {
		return true
	}
	for _, ch := range channels {
		if ch.IsBroadcasting() {
			return true
		}
	}
	return false
}

// hasBroadcastingChannel reports whether any channel is currently broadcasting.
func (c *Client) hasBroadcastingChannel() bool {
	for _, ch := range c.mgr.List() {
		if ch.IsBroadcasting() {
			return true
		}
	}
	return false
}

const stopTimeout = 3 * time.Second

// Stop signals the client to shut down and waits for quit to be sent to the YP.
func (c *Client) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(stopTimeout):
		slog.Warn("yp: stop timed out", "addr", c.addr)
	}
}

// Bump triggers an immediate bcst send to the YP for all active channels.
func (c *Client) Bump() {
	select {
	case c.bumpCh <- struct{}{}:
	default:
	}
}

func (c *Client) run() (connected bool, err error) {
	conn, err := pcp.Dial(c.addr)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	if tcp, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		c.localIP, _ = pcp.IPv4ToUint32(tcp.IP)
	}

	// Send helo.
	if err := conn.WriteAtom(c.buildHelo()); err != nil {
		return false, fmt.Errorf("write helo: %w", err)
	}

	updateInterval := defaultInterval
	sendImmediately := false

	// Read oleh + optional root + ok.
	for {
		a, err := conn.ReadAtom()
		if err != nil {
			return false, fmt.Errorf("read handshake: %w", err)
		}
		switch a.Tag {
		case pcp.PCPOleh:
			c.handleOleh(a)
		case pcp.PCPRoot:
			interval, immediate := parseRoot(a)
			if interval > 0 {
				updateInterval = time.Duration(interval) * time.Second
			}
			if immediate {
				sendImmediately = true
			}
		case pcp.PCPOK:
			goto handshakeDone
		case pcp.PCPQuit:
			return false, fmt.Errorf("quit from YP during handshake")
		}
	}

handshakeDone:
	slog.Info("yp: connected", "addr", c.addr, "global_ip", ipToString(c.globalIP), "update_interval", updateInterval)

	// Relay-only nodes have nothing to announce. We only connected to learn
	// our global IP from the oleh response; now that we have it, disconnect
	// cleanly. Otherwise the YP would close the connection after its read
	// timeout (UpdateInterval + 60s) since we never send any bcst.
	if !c.hasBroadcastingChannel() {
		_ = conn.WriteAtom(pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown))
		slog.Info("yp: disconnected (relay-only, global IP learned)", "addr", c.addr)
		return true, nil
	}

	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	// Send initial bcst for all active channels. The handshake-phase
	// sendImmediately flag is honored implicitly here: regardless of its value
	// we perform an initial announcement right after the handshake.
	_ = sendImmediately
	if err := c.sendAllBcst(conn); err != nil {
		return true, fmt.Errorf("write bcst: %w", err)
	}

	// Post-handshake reader: listens for root/quit atoms from the YP.
	// On a root atom with the update flag set, trigger an immediate bcst.
	// On a quit atom or any read error, signal the main loop to exit.
	readErrCh := make(chan error, 1)
	immediateCh := make(chan struct{}, 1)
	go func() {
		for {
			a, err := conn.ReadAtom()
			if err != nil {
				readErrCh <- err
				return
			}
			switch a.Tag {
			case pcp.PCPRoot:
				if _, immediate := parseRoot(a); immediate {
					select {
					case immediateCh <- struct{}{}:
					default:
					}
				}
			case pcp.PCPQuit:
				readErrCh <- fmt.Errorf("quit from YP")
				return
			}
		}
	}()

	for {
		select {
		case <-c.stopCh:
			_ = conn.WriteAtom(pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown))
			slog.Info("yp: disconnected", "addr", c.addr)
			return true, nil
		case err := <-readErrCh:
			return true, fmt.Errorf("read: %w", err)
		case <-ticker.C:
			if err := c.sendAllBcst(conn); err != nil {
				return true, fmt.Errorf("write bcst: %w", err)
			}
		case <-immediateCh:
			if err := c.sendAllBcst(conn); err != nil {
				return true, fmt.Errorf("write bcst (immediate): %w", err)
			}
			slog.Debug("yp: bcst sent (root update)", "addr", c.addr)
		case <-c.bumpCh:
			if err := c.sendAllBcst(conn); err != nil {
				return true, fmt.Errorf("write bcst (bump): %w", err)
			}
			slog.Debug("yp: bcst sent (bump)", "addr", c.addr)
		}
	}
}

// sendAllBcst sends one bcst atom per broadcasting (non-relay) channel.
func (c *Client) sendAllBcst(conn *pcp.Conn) error {
	channels := c.mgr.List()
	count := 0
	for _, ch := range channels {
		if !ch.IsBroadcasting() {
			continue
		}
		if err := conn.WriteAtom(c.buildBcst(ch)); err != nil {
			return err
		}
		count++
	}
	if count > 0 {
		slog.Debug("yp: bcst sent", "addr", c.addr, "channels", count)
	}
	return nil
}

func (c *Client) handleOleh(a *pcp.Atom) {
	oleh, err := pcp.ParseHeloPacket(a)
	if err != nil {
		return
	}
	if oleh.RemoteIP != 0 {
		c.globalIP = oleh.RemoteIP
		slog.Debug("yp: oleh received", "addr", c.addr, "global_ip", ipToString(oleh.RemoteIP))
		if c.OnGlobalIP != nil {
			c.OnGlobalIP(oleh.RemoteIP)
		}
	}
}

func (c *Client) buildHelo() *pcp.Atom {
	h := pcp.HeloPacket{
		Agent:     version.AgentName,
		Version:   version.PCPVersion,
		SessionID: c.sessionID,
		Port:      c.listenPort,
		Ping:      c.listenPort,
		BCID:      c.broadcastID,
	}
	return h.BuildHeloAtom()
}

func (c *Client) buildBcst(ch *channel.Channel) *pcp.Atom {
	info := ch.Info()
	track := ch.Track()

	ci := info.ToPCP()
	ct := track.ToPCP()
	chanAtom := (&pcp.ChanPacket{
		ID:          ch.ID,
		BroadcastID: ch.BroadcastID(),
		Info:        &ci,
		Track:       &ct,
	}).BuildAtom()

	hp := pcputil.HostAtomParams{
		SessionID:    c.sessionID,
		LocalIP:      c.localIP,
		GlobalIP:     c.globalIP,
		ListenPort:   c.listenPort,
		ChannelID:    ch.ID,
		NumListeners: ch.NumListeners(),
		NumRelays:    ch.NumRelays(),
		Uptime:       ch.UptimeSeconds(),
		OldPos:       ch.OldestPos(),
		NewPos:       ch.NewestPos(),
		IsTracker:    true,
		HasGlobalIP:  true,
		TrackerAtom:  true,
		RelayFull:    ch.IsRelayFull(c.maxRelays),
		DirectFull:   ch.IsDirectFull(c.maxListeners),
	}
	if upAddr := ch.UpstreamAddr(); upAddr != "" {
		if upIP, upPort, err := parseHostPort(upAddr); err == nil {
			hp.UphostIP = upIP
			hp.UphostPort = upPort
		}
	}
	hostAtom := pcputil.BuildHostAtom(hp)

	return pcp.NewParentAtom(pcp.PCPBcst,
		pcp.NewByteAtom(pcp.PCPBcstTTL, bcstTTL),
		pcp.NewByteAtom(pcp.PCPBcstHops, 0),
		pcp.NewIDAtom(pcp.PCPBcstFrom, c.sessionID),
		pcp.NewByteAtom(pcp.PCPBcstGroup, byte(pcp.PCPBcstGroupRoot)),
		pcp.NewIDAtom(pcp.PCPBcstChanID, ch.ID),
		pcp.NewIntAtom(pcp.PCPBcstVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPBcstVersionVP, version.PCPVersionVP),
		pcp.NewBytesAtom(pcp.PCPBcstVersionExPrefix, []byte(version.ExPrefix)),
		pcp.NewShortAtom(pcp.PCPBcstVersionExNumber, version.ExNumber()),
		chanAtom,
		hostAtom,
	)
}

func parseRoot(a *pcp.Atom) (interval uint32, immediate bool) {
	root, err := pcp.ParseRootPacket(a)
	if err != nil {
		return
	}
	return root.UpdateInterval, root.Update != nil
}

func ipToString(ip uint32) string {
	return pcp.IPv4FromUint32(ip).String()
}

// parseHostPort parses "host:port" and returns the IP as a big-endian uint32
// and the port number. Returns an error for non-IPv4 or invalid addresses.
func parseHostPort(addr string) (ip uint32, port uint16, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, 0, err
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return 0, 0, fmt.Errorf("resolve %s: %w", host, err)
	}
	v4 := ips[0].To4()
	if v4 == nil {
		return 0, 0, fmt.Errorf("not IPv4: %s", host)
	}
	ip, err = pcp.IPv4ToUint32(v4)
	if err != nil {
		return 0, 0, fmt.Errorf("IPv4ToUint32: %w", err)
	}
	p, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return 0, 0, err
	}
	return ip, uint16(p), nil
}
