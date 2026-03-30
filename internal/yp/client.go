package yp

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/version"
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

	stopCh chan struct{}
	bumpCh chan struct{}
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
	}
}

// Run connects to the YP and runs the bcst loop, reconnecting on failure.
func (c *Client) Run() {
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

// Stop signals the client to shut down.
func (c *Client) Stop() {
	close(c.stopCh)
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

// sendAllBcst sends one bcst atom per active channel.
func (c *Client) sendAllBcst(conn *pcp.Conn) error {
	channels := c.mgr.List()
	for _, ch := range channels {
		if err := conn.WriteAtom(c.buildBcst(ch)); err != nil {
			return err
		}
	}
	slog.Debug("yp: bcst sent", "addr", c.addr, "channels", len(channels))
	return nil
}

func (c *Client) handleOleh(a *pcp.Atom) {
	rip := a.FindChild(pcp.PCPHeloRemoteIP)
	if rip != nil {
		if v, err := rip.GetInt(); err == nil {
			c.globalIP = v
			slog.Debug("yp: oleh received", "addr", c.addr, "global_ip", ipToString(v))
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
	buf := ch.Buffer

	chanInfo := pcp.NewParentAtom(pcp.PCPChanInfo,
		pcp.NewStringAtom(pcp.PCPChanInfoName, info.Name),
		pcp.NewStringAtom(pcp.PCPChanInfoURL, info.URL),
		pcp.NewStringAtom(pcp.PCPChanInfoDesc, info.Desc),
		pcp.NewStringAtom(pcp.PCPChanInfoComment, info.Comment),
		pcp.NewStringAtom(pcp.PCPChanInfoGenre, info.Genre),
		pcp.NewStringAtom(pcp.PCPChanInfoType, info.Type),
		pcp.NewIntAtom(pcp.PCPChanInfoBitrate, info.Bitrate),
	)

	chanTrack := pcp.NewParentAtom(pcp.PCPChanTrack,
		pcp.NewStringAtom(pcp.PCPChanTrackTitle, track.Title),
		pcp.NewStringAtom(pcp.PCPChanTrackCreator, track.Creator),
		pcp.NewStringAtom(pcp.PCPChanTrackURL, track.URL),
		pcp.NewStringAtom(pcp.PCPChanTrackAlbum, track.Album),
	)

	chanAtom := pcp.NewParentAtom(pcp.PCPChan,
		pcp.NewIDAtom(pcp.PCPChanID, ch.ID),
		pcp.NewIDAtom(pcp.PCPChanBCID, ch.BroadcastID),
		chanInfo,
		chanTrack,
	)

	flags := byte(pcp.PCPHostFlags1Tracker | pcp.PCPHostFlags1Relay | pcp.PCPHostFlags1Direct | pcp.PCPHostFlags1Recv | pcp.PCPHostFlags1CIN)
	hostAtom := pcp.NewParentAtom(pcp.PCPHost,
		pcp.NewIDAtom(pcp.PCPHostID, c.sessionID),
		pcp.NewIntAtom(pcp.PCPHostIP, c.globalIP),
		pcp.NewShortAtom(pcp.PCPHostPort, c.listenPort),
		pcp.NewIntAtom(pcp.PCPHostNumListeners, uint32(ch.NumListeners())),
		pcp.NewIntAtom(pcp.PCPHostNumRelays, uint32(ch.NumRelays())),
		pcp.NewIntAtom(pcp.PCPHostUptime, ch.UptimeSeconds()),
		pcp.NewIntAtom(pcp.PCPHostOldPos, buf.OldestPos()),
		pcp.NewIntAtom(pcp.PCPHostNewPos, buf.NewestPos()),
		pcp.NewIDAtom(pcp.PCPHostChanID, ch.ID),
		pcp.NewByteAtom(pcp.PCPHostFlags1, flags),
		pcp.NewIntAtom(pcp.PCPHostTracker, 1),
		pcp.NewIntAtom(pcp.PCPHostVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPHostVersionVP, version.PCPVersionVP),
		pcp.NewBytesAtom(pcp.PCPHostVersionExPrefix, []byte(version.ExPrefix)),
		pcp.NewShortAtom(pcp.PCPHostVersionExNumber, version.ExNumber()),
	)

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
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, ip)
	return net.IP(b).String()
}
