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
	bcstTTL         = 7
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
	addr        string
	sessionID   pcp.GnuID
	broadcastID pcp.GnuID
	mgr         ChannelLister
	listenPort  uint16

	globalIP uint32 // learned from oleh.rip

	// OnGlobalIP is called with the global IP address learned from the YP oleh.
	// May be nil.
	OnGlobalIP func(uint32)

	stopCh   chan struct{}
	bumpCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// New creates a new YPClient.
func New(addr string, sessionID, broadcastID pcp.GnuID, mgr ChannelLister, listenPort int) *Client {
	if listenPort <= 0 {
		listenPort = defaultPCPPort
	}
	return &Client{
		addr:        addr,
		sessionID:   sessionID,
		broadcastID: broadcastID,
		mgr:         mgr,
		listenPort:  uint16(listenPort),
		stopCh:      make(chan struct{}),
		bumpCh:      make(chan struct{}, 1),
		doneCh:      make(chan struct{}),
	}
}

// Run connects to the YP and runs the bcst loop, reconnecting on failure.
// It waits until at least one broadcasting channel exists before connecting.
func (c *Client) Run() {
	defer close(c.doneCh)
	if !c.waitForBroadcast() {
		return
	}
	delay := retryInitial
	for {
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

// waitForBroadcast blocks until there is at least one broadcasting channel,
// or until the client is stopped. Returns false if stopped before any channel appeared.
func (c *Client) waitForBroadcast() bool {
	for {
		for _, ch := range c.mgr.List() {
			if ch.IsBroadcasting() {
				return true
			}
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

	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	// Send initial bcst for all active channels.
	_ = sendImmediately
	if err := c.sendAllBcst(conn); err != nil {
		return true, fmt.Errorf("write bcst: %w", err)
	}

	for {
		select {
		case <-c.stopCh:
			_ = conn.WriteAtom(pcp.NewIntAtom(pcp.PCPQuit, pcp.PCPErrorQuit+pcp.PCPErrorShutdown))
			slog.Info("yp: disconnected", "addr", c.addr)
			return true, nil
		case <-ticker.C:
			if err := c.sendAllBcst(conn); err != nil {
				return true, fmt.Errorf("write bcst: %w", err)
			}
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
	rip := a.FindChild(pcp.PCPHeloRemoteIP)
	if rip != nil {
		if v, err := rip.GetInt(); err == nil {
			c.globalIP = v
			slog.Debug("yp: oleh received", "addr", c.addr, "global_ip", ipToString(v))
			if c.OnGlobalIP != nil {
				c.OnGlobalIP(v)
			}
		}
	}
}

func (c *Client) buildHelo() *pcp.Atom {
	return pcp.NewParentAtom(pcp.PCPHelo,
		pcp.NewStringAtom(pcp.PCPHeloAgent, version.AgentName),
		pcp.NewIntAtom(pcp.PCPHeloVersion, version.PCPVersion),
		pcp.NewIDAtom(pcp.PCPHeloSessionID, c.sessionID),
		pcp.NewShortAtom(pcp.PCPHeloPort, c.listenPort),
		pcp.NewShortAtom(pcp.PCPHeloPing, c.listenPort),
		pcp.NewIDAtom(pcp.PCPHeloBCID, c.broadcastID),
	)
}

func (c *Client) buildBcst(ch *channel.Channel) *pcp.Atom {
	info := ch.Info()
	track := ch.Track()

	ci := info.ToPCP()
	chanInfo := ci.BuildAtom()

	ct := track.ToPCP()
	chanTrack := ct.BuildAtom()

	chanAtom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, ch.ID),
		pcp.NewIDAtom(pcp.PCPChanBCID, ch.BroadcastID()),
		chanInfo,
		chanTrack,
	)

	hp := pcputil.HostAtomParams{
		SessionID:    c.sessionID,
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
		chanAtom,
		hostAtom,
	)
}

func parseRoot(a *pcp.Atom) (interval uint32, immediate bool) {
	if v := a.FindChild(pcp.PCPRootUpdInt); v != nil {
		if n, err := v.GetInt(); err == nil {
			interval = n
		}
	}
	if a.FindChild(pcp.PCPRootUpdate) != nil {
		immediate = true
	}
	return
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
	ip = pcp.IPv4ToUint32(v4)
	p, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return 0, 0, err
	}
	return ip, uint16(p), nil
}
