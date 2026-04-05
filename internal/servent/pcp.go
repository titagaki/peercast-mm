package servent

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/channel"
	"github.com/titagaki/peercast-mi/internal/pcputil"
	"github.com/titagaki/peercast-mi/internal/version"
)

const (
	outputQueueTimeout  = 5 * time.Second
	pcpWriteTimeout     = 10 * time.Second
	pcpHandshakeTimeout = 18 * time.Second // PeerCastStation 互換
	maxContentBodyLen   = 15 * 1024        // PeerCastStation 互換: 15KB 超のパケットを分割
)

// PCPOutputStream sends PCP stream data to a downstream relay node.
type PCPOutputStream struct {
	outputBase
	br           *bufio.Reader
	sessionID    pcp.GnuID
	peerID       pcp.GnuID // 下流ピアの session ID（ループ防止用）
	ch           *channel.Channel
	bcstCh       chan *pcp.Atom
	globalIP     uint32
	listenPort   uint16
	remotePort   uint16 // 下流ピアのポート（0 = firewalled）
	maxRelays    int    // per-channel 制限 (0 = unlimited)
	maxListeners int    // per-channel 制限 (0 = unlimited)
}

func newPCPOutputStream(conn *countingConn, br *bufio.Reader, sessionID pcp.GnuID, ch *channel.Channel, id int, globalIP uint32, listenPort uint16, maxRelays, maxListeners int) *PCPOutputStream {
	return &PCPOutputStream{
		outputBase:   newOutputBase(conn, id),
		br:           br,
		sessionID:    sessionID,
		ch:           ch,
		bcstCh:       make(chan *pcp.Atom, 8),
		globalIP:     globalIP,
		listenPort:   listenPort,
		maxRelays:    maxRelays,
		maxListeners: maxListeners,
	}
}

// Type implements channel.OutputStream.
func (o *PCPOutputStream) Type() channel.OutputStreamType { return channel.OutputStreamPCP }

// PeerID implements channel.OutputStream.
func (o *PCPOutputStream) PeerID() pcp.GnuID { return o.peerID }

// IsFirewalled reports whether the downstream peer has no open port.
func (o *PCPOutputStream) IsFirewalled() bool { return o.remotePort == 0 }

// SendBcst enqueues a bcst atom for forwarding to this downstream peer.
func (o *PCPOutputStream) SendBcst(atom *pcp.Atom) {
	select {
	case o.bcstCh <- atom:
	default:
	}
}

// runStreaming sends initial data and enters the stream loop.
// It is called after a successful handshake and admission.
func (o *PCPOutputStream) runStreaming(startPos uint32) {
	defer slog.Info("pcp: relay disconnected", "remote", o.remoteAddr, "id", o.id)
	defer o.conn.Close()

	if err := o.sendInitial(); err != nil {
		slog.Error("pcp: send initial error", "remote", o.remoteAddr, "id", o.id, "err", err)
		return
	}

	go o.readLoop()
	o.streamLoop(startPos)
}

// handshake processes the HTTP GET and helo/oleh exchange.
// admitted indicates whether the relay was accepted; if false, an HTTP 503 is
// sent instead of 200 and PCP_OK is not sent.
// It returns the requested start position from the x-peercast-pos header (0 if absent).
//
// PCP over HTTP の場合、クライアントは HTTP 200 受信後に pcp\n マジックを送らず
// 直接 helo アトムを送る（peercast-yt channel.cpp "don't need PCP_CONNECT here"）。
func (o *PCPOutputStream) handshake(admitted bool) (startPos uint32, err error) {
	// Apply handshake timeout (PeerCastStation 互換: 18秒).
	o.conn.SetDeadline(time.Now().Add(pcpHandshakeTimeout))
	defer o.conn.SetDeadline(time.Time{})

	// Read HTTP request.
	req, err := http.ReadRequest(o.br)
	if err != nil {
		return 0, fmt.Errorf("read HTTP request: %w", err)
	}
	_ = req.Body.Close()

	// x-peercast-pos ヘッダーを読み取る
	if v := req.Header.Get("x-peercast-pos"); v != "" {
		if n, parseErr := strconv.ParseUint(v, 10, 32); parseErr == nil {
			startPos = uint32(n)
		}
	}

	// Send HTTP response: 200 if admitted, 503 if relay full (PeerCastStation 互換).
	var resp string
	if admitted {
		resp = "HTTP/1.0 200 OK\r\nContent-Type: application/x-peercast-pcp\r\n\r\n"
	} else {
		resp = "HTTP/1.0 503 Unavailable\r\nContent-Type: application/x-peercast-pcp\r\n\r\n"
	}
	if _, err := io.WriteString(o.conn, resp); err != nil {
		return 0, fmt.Errorf("write HTTP response: %w", err)
	}

	// Read helo.
	heloAtom, err := pcp.ReadAtom(o.br)
	if err != nil {
		return 0, fmt.Errorf("read helo: %w", err)
	}
	if heloAtom.Tag != pcp.PCPHelo {
		return 0, fmt.Errorf("expected helo, got %s", heloAtom.Tag)
	}

	// Parse and validate helo.
	helo, err := pcp.ParseHeloPacket(heloAtom)
	if err != nil {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
		return 0, fmt.Errorf("parse helo: %w", err)
	}
	peerID := helo.SessionID
	o.peerID = peerID
	if peerID == o.sessionID {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorLoopback)
		return 0, fmt.Errorf("loopback connection")
	}
	var zeroID pcp.GnuID
	if peerID == zeroID {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorNotIdentified)
		return 0, fmt.Errorf("not identified")
	}
	if helo.Version == 0 {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorBadAgent)
		return 0, fmt.Errorf("no version in helo")
	}
	if helo.Version < 1200 {
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorBadAgent)
		return 0, fmt.Errorf("bad agent version %d", helo.Version)
	}

	// Determine remote port from helo: ping (active check) > port (claimed).
	remoteIP := ipToUint32(o.conn.RemoteAddr())
	var remotePort uint16
	if helo.Ping != 0 {
		remoteAddr := o.conn.RemoteAddr().(*net.TCPAddr)
		// Site-local addresses are not reachable from the outside, so even
		// if the ping succeeds we treat the port as closed (PeerCastStation 互換).
		if !isSiteLocal(remoteAddr.IP) && pingHost(remoteAddr.IP, helo.Ping, peerID, o.sessionID) {
			remotePort = helo.Ping
		}
	} else if helo.Port != 0 {
		remotePort = helo.Port
	}

	o.remotePort = remotePort

	// Send oleh.
	oleh := (&pcp.HeloPacket{
		Agent:     version.AgentName,
		SessionID: o.sessionID,
		Version:   version.PCPVersion,
		RemoteIP:  remoteIP,
		Port:      remotePort,
	}).BuildOlehAtom()
	if err := oleh.Write(o.conn); err != nil {
		return 0, fmt.Errorf("write oleh: %w", err)
	}

	// Send PCP_OK only when the relay is accepted (PeerCastStation 互換).
	if admitted {
		if err := pcp.NewIntAtom(pcp.PCPOK, 1).Write(o.conn); err != nil {
			return 0, fmt.Errorf("write ok: %w", err)
		}
	}

	return startPos, nil
}

// sendRelayDenied sends the host atom followed by up to 8 alternative relay
// candidates and a QUIT with PCPErrorUnavailable, used when the relay limit
// has been reached (PeerCastStation 互換: SelectSourceHosts で候補を返す)。
func (o *PCPOutputStream) sendRelayDenied() {
	hostAtom := o.buildHostAtom()
	hostAtom.Write(o.conn)
	for _, alt := range o.ch.SelectSourceHosts(8) {
		if err := alt.Write(o.conn); err != nil {
			break
		}
	}
	o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorUnavailable)
}

// sendInitial sends chan (info, trck, head pkt) and host atoms.
func (o *PCPOutputStream) sendInitial() error {
	info := o.ch.Info()
	track := o.ch.Track()
	header, headerPos := o.ch.Header()

	chanAtom := buildChanAtom(o.ch.ID, o.ch.BroadcastID(), info, track, header, headerPos)
	hostAtom := o.buildHostAtom()

	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	if err := chanAtom.Write(o.conn); err != nil {
		o.conn.SetWriteDeadline(time.Time{})
		return err
	}
	err := hostAtom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

func (o *PCPOutputStream) buildHostAtom() *pcp.Atom {
	var localIP uint32
	if tcp, ok := o.conn.LocalAddr().(*net.TCPAddr); ok {
		localIP, _ = pcp.IPv4ToUint32(tcp.IP)
	}
	return pcputil.BuildHostAtom(pcputil.HostAtomParams{
		SessionID:    o.sessionID,
		LocalIP:      localIP,
		GlobalIP:     o.globalIP,
		ListenPort:   o.listenPort,
		ChannelID:    o.ch.ID,
		NumListeners: o.ch.NumListeners(),
		NumRelays:    o.ch.NumRelays(),
		Uptime:       o.ch.UptimeSeconds(),
		OldPos:       o.ch.OldestPos(),
		NewPos:       o.ch.NewestPos(),
		IsTracker:    o.ch.IsBroadcasting(),
		HasGlobalIP:  o.globalIP != 0,
		RelayFull:    o.ch.IsRelayFull(o.maxRelays),
		DirectFull:   o.ch.IsDirectFull(o.maxListeners),
	})
}

// streamLoop continuously sends buffered content packets to the peer.
// reqPos は x-peercast-pos で指定された開始位置 (0 = ヘッダー位置から開始)。
func (o *PCPOutputStream) streamLoop(reqPos uint32) {
	_, hpos := o.ch.Header()
	pos := hpos
	// reqPos == 0 は「未指定」と同義に扱う。ストリーム開始直後に pos=0 を送ってくる
	// クライアントがいても hpos == 0 のはずなので実害はない。
	if reqPos > 0 {
		oldest := o.ch.OldestPos()
		if reqPos >= oldest {
			pos = reqPos
		} else {
			pos = oldest
		}
	}

	stallTimer := time.NewTimer(outputQueueTimeout)
	defer stallTimer.Stop()
	waitingForKeyframe := true

	for {
		// Process notifications non-blockingly.
		if err := o.drainNotifications(); err != nil {
			slog.Debug("pcp: notification write error, closing", "remote", o.remoteAddr, "id", o.id, "err", err)
			return
		}

		// Send buffered data packets.
		sigCh := o.ch.Signal()
		packets := o.ch.Since(pos)

		if len(packets) > 0 {
			var err error
			pos, waitingForKeyframe, err = o.sendDataPackets(packets, pos, waitingForKeyframe)
			if err != nil {
				return
			}
			stallTimer.Reset(outputQueueTimeout)
			continue
		}

		select {
		case <-o.closeCh:
			slog.Debug("pcp: closed by readLoop", "remote", o.remoteAddr, "id", o.id)
			o.sendUpstreamHostAndQuit(pcp.PCPErrorQuit + pcp.PCPErrorShutdown)
			return
		case <-sigCh:
			// New data written to buffer.
		case <-o.infoCh:
			if err := o.sendInfoUpdate(); err != nil {
				slog.Debug("pcp: info write error, closing", "remote", o.remoteAddr, "id", o.id, "err", err)
				return
			}
		case <-o.trackCh:
			if err := o.sendTrackUpdate(); err != nil {
				slog.Debug("pcp: track write error, closing", "remote", o.remoteAddr, "id", o.id, "err", err)
				return
			}
		case <-o.headerCh:
			if err := o.sendHeaderUpdate(); err != nil {
				slog.Debug("pcp: header write error, closing", "remote", o.remoteAddr, "id", o.id, "err", err)
				return
			}
		case atom := <-o.bcstCh:
			if err := o.sendBcstAtom(atom); err != nil {
				slog.Debug("pcp: bcst write error, closing", "remote", o.remoteAddr, "id", o.id, "err", err)
				return
			}
		case <-stallTimer.C:
			slog.Info("pcp: queue timeout, closing", "remote", o.remoteAddr, "id", o.id)
			return
		}
	}
}

// sendDataPackets writes buffered content packets to the downstream peer.
// It skips non-keyframe packets until the first keyframe is found.
// Returns the updated stream position, keyframe state, and any write error.
func (o *PCPOutputStream) sendDataPackets(packets []channel.Content, pos uint32, waitingForKeyframe bool) (uint32, bool, error) {
	// Overflow detection: if the oldest unsent packet was written > 5s ago,
	// the downstream is too slow (PeerCastStation 互換).
	if !packets[0].Timestamp.IsZero() && time.Since(packets[0].Timestamp) > outputQueueTimeout {
		slog.Info("pcp: send overflow, closing", "remote", o.remoteAddr, "id", o.id,
			"delay", time.Since(packets[0].Timestamp))
		o.sendQuit(pcp.PCPErrorQuit + pcp.PCPErrorSkip)
		return pos, waitingForKeyframe, fmt.Errorf("overflow")
	}
	if len(packets) > 10 {
		slog.Debug("pcp: sending burst", "remote", o.remoteAddr, "id", o.id, "packets", len(packets), "pos", pos)
	}
	for _, pkt := range packets {
		if waitingForKeyframe && pkt.ContFlags != 0 {
			pos = pkt.Pos + uint32(len(pkt.Data))
			continue
		}
		waitingForKeyframe = false

		// Split large packets into maxContentBodyLen chunks.
		// The first chunk keeps the original ContFlags; subsequent chunks
		// get the Fragment flag (PeerCastStation 互換).
		data := pkt.Data
		chunkPos := pkt.Pos
		contFlags := pkt.ContFlags
		for len(data) > 0 {
			chunk := data
			if len(chunk) > maxContentBodyLen {
				chunk = data[:maxContentBodyLen]
			}
			atom := (&pcp.ChanPacket{
				ID: o.ch.ID,
				Pkt: &pcp.ChanPktData{
					Type:         pcp.NewID4("data"),
					Pos:          chunkPos,
					Data:         chunk,
					Continuation: contFlags,
				},
			}).BuildAtom()
			o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
			if err := atom.Write(o.conn); err != nil {
				slog.Debug("pcp: write error, closing", "remote", o.remoteAddr, "id", o.id, "pos", chunkPos, "err", err)
				return pos, waitingForKeyframe, err
			}
			o.conn.SetWriteDeadline(time.Time{})
			data = data[len(chunk):]
			chunkPos += uint32(len(chunk))
			// Subsequent chunks OR the Fragment flag onto the original flags
			// (PeerCastStation 互換: pkt.ContFlag | Fragment)。上書きすると
			// InterFrame/AudioFrame などの元フラグが失われる。
			contFlags = pkt.ContFlags | 0x01
		}
		pos = pkt.Pos + uint32(len(pkt.Data))
	}
	return pos, waitingForKeyframe, nil
}

// drainNotifications processes any pending info/track/header/bcst notifications
// without blocking. Returns an error if any write fails.
func (o *PCPOutputStream) drainNotifications() error {
	for {
		select {
		case <-o.infoCh:
			if err := o.sendInfoUpdate(); err != nil {
				return err
			}
		case <-o.trackCh:
			if err := o.sendTrackUpdate(); err != nil {
				return err
			}
		case <-o.headerCh:
			if err := o.sendHeaderUpdate(); err != nil {
				return err
			}
		case atom := <-o.bcstCh:
			if err := o.sendBcstAtom(atom); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// sendInfoUpdate sends the current ChannelInfo to the downstream peer. For
// broadcasting channels it wraps the chan atom in a bcst container so that
// downstream nodes can forward the info update throughout the relay tree
// (PeerCastStation 互換: SendRelayBody → BcstChannelInfo).
func (o *PCPOutputStream) sendInfoUpdate() error {
	ci := o.ch.Info().ToPCP()
	chanAtom := (&pcp.ChanPacket{ID: o.ch.ID, Info: &ci}).BuildAtom()
	atom := o.wrapBcstIfBroadcasting(chanAtom)
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	err := atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

func (o *PCPOutputStream) sendTrackUpdate() error {
	ct := o.ch.Track().ToPCP()
	chanAtom := (&pcp.ChanPacket{ID: o.ch.ID, Track: &ct}).BuildAtom()
	atom := o.wrapBcstIfBroadcasting(chanAtom)
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	err := atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

// wrapBcstIfBroadcasting wraps a chan atom in a bcst container when the
// channel is locally broadcasting. Otherwise the atom is returned as-is.
func (o *PCPOutputStream) wrapBcstIfBroadcasting(chanAtom *pcp.Atom) *pcp.Atom {
	if !o.ch.IsBroadcasting() {
		return chanAtom
	}
	children := []*pcp.Atom{
		pcp.NewByteAtom(pcp.PCPBcstTTL, 11),
		pcp.NewByteAtom(pcp.PCPBcstHops, 0),
		pcp.NewIDAtom(pcp.PCPBcstFrom, o.sessionID),
		pcp.NewByteAtom(pcp.PCPBcstGroup, pcp.PCPBcstGroupRelays),
		pcp.NewIDAtom(pcp.PCPBcstChanID, o.ch.ID),
		pcp.NewIntAtom(pcp.PCPBcstVersion, version.PCPVersion),
		pcp.NewIntAtom(pcp.PCPBcstVersionVP, version.PCPVersionVP),
		pcp.NewBytesAtom(pcp.PCPBcstVersionExPrefix, []byte(version.ExPrefix)),
		pcp.NewShortAtom(pcp.PCPBcstVersionExNumber, version.ExNumber()),
		chanAtom,
	}
	return pcp.NewParentAtom(pcp.PCPBcst, children...)
}

func (o *PCPOutputStream) sendHeaderUpdate() error {
	header, hpos := o.ch.Header()
	atom := (&pcp.ChanPacket{
		ID: o.ch.ID,
		Pkt: &pcp.ChanPktData{
			Type: pcp.NewID4("head"),
			Pos:  hpos,
			Data: header,
		},
	}).BuildAtom()
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	err := atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

func (o *PCPOutputStream) sendBcstAtom(atom *pcp.Atom) error {
	o.conn.SetWriteDeadline(time.Now().Add(pcpWriteTimeout))
	err := atom.Write(o.conn)
	o.conn.SetWriteDeadline(time.Time{})
	return err
}

// readLoop reads atoms from the downstream peer in a dedicated goroutine.
// o.br (not o.conn) must be used because o.br may have buffered bytes from the handshake.
// On any read error or quit atom, it closes the stream so streamLoop exits.
func (o *PCPOutputStream) readLoop() {
	for {
		a, err := pcp.ReadAtom(o.br)
		if err != nil {
			slog.Debug("pcp: read error from downstream", "remote", o.remoteAddr, "id", o.id, "err", err)
			o.Close()
			return
		}
		switch a.Tag {
		case pcp.PCPBcst:
			o.forwardBcst(a)
		case pcp.PCPQuit:
			code := uint32(0)
			if v, err := a.GetInt(); err == nil {
				code = v
			}
			slog.Debug("pcp: quit received from downstream", "remote", o.remoteAddr, "id", o.id, "code", code)
			o.Close()
			return
		default:
			slog.Debug("pcp: unknown atom from downstream", "remote", o.remoteAddr, "id", o.id, "tag", a.Tag)
		}
	}
}

func (o *PCPOutputStream) forwardBcst(a *pcp.Atom) {
	// TTL decrement: find ttl child.
	ttlAtom := a.FindChild(pcp.PCPBcstTTL)
	if ttlAtom == nil {
		return
	}
	ttl, err := ttlAtom.GetByte()
	if err != nil || ttl == 0 {
		return
	}
	// Check from == our sessionID (loop prevention).
	if from := a.FindChild(pcp.PCPBcstFrom); from != nil {
		id, err := from.GetID()
		if err == nil && id == o.sessionID {
			return
		}
	}
	// Check dest: if set and not us, forward without processing.
	if dest := a.FindChild(pcp.PCPBcstDest); dest != nil {
		id, err := dest.GetID()
		if err == nil && id == o.sessionID {
			return // addressed to us, don't forward
		}
	}
	// Cache any Host atom payload so that SelectSourceHosts can hand it out
	// as an alternative relay candidate when this node is full.
	if host := a.FindChild(pcp.PCPHost); host != nil {
		o.ch.AddKnownHost(host)
	}
	// Rebuild bcst with decremented TTL and incremented hops.
	forwarded := rebuildBcst(a, ttl)
	o.ch.Broadcast(o, forwarded)
	slog.Debug("pcp: bcst forwarded from downstream", "remote", o.remoteAddr, "id", o.id, "ttl", ttl-1)
}

// rebuildBcst creates a new bcst atom with TTL decremented by 1 and hops incremented by 1.
func rebuildBcst(a *pcp.Atom, ttl byte) *pcp.Atom {
	var children []*pcp.Atom
	for _, c := range a.Children() {
		switch c.Tag {
		case pcp.PCPBcstTTL:
			children = append(children, pcp.NewByteAtom(pcp.PCPBcstTTL, ttl-1))
		case pcp.PCPBcstHops:
			hops, err := c.GetByte()
			if err == nil {
				children = append(children, pcp.NewByteAtom(pcp.PCPBcstHops, hops+1))
			} else {
				children = append(children, c)
			}
		default:
			children = append(children, c)
		}
	}
	return pcp.NewParentAtom(pcp.PCPBcst, children...)
}

func (o *PCPOutputStream) sendQuit(code uint32) {
	q := pcp.NewIntAtom(pcp.PCPQuit, code)
	q.Write(o.conn)
}

// sendUpstreamHostAndQuit sends the upstream node info as a HOST atom (so the
// downstream can reconnect directly to the upstream) followed by QUIT.
// PeerCastStation 互換: BeforeQuitAsync で上流ノード情報を返す。
func (o *PCPOutputStream) sendUpstreamHostAndQuit(code uint32) {
	upSID, upIP, upPort := o.ch.UpstreamNodeInfo()
	var zeroID pcp.GnuID
	if upSID != zeroID && upIP != 0 {
		host := pcputil.BuildHostAtom(pcputil.HostAtomParams{
			SessionID:  upSID,
			GlobalIP:   upIP,
			ListenPort: upPort,
			ChannelID:  o.ch.ID,
		})
		host.Write(o.conn)
	}
	o.sendQuit(code)
}

// ---------------------------------------------------------------------------
// Atom builders
// ---------------------------------------------------------------------------

func buildChanAtom(chanID, bcID pcp.GnuID, info channel.ChannelInfo, track channel.TrackInfo, header []byte, headerPos uint32) *pcp.Atom {
	ci := info.ToPCP()
	ct := track.ToPCP()
	return (&pcp.ChanPacket{
		ID:          chanID,
		BroadcastID: bcID,
		Info:        &ci,
		Track:       &ct,
		Pkt: &pcp.ChanPktData{
			Type: pcp.NewID4("head"),
			Pos:  headerPos,
			Data: header,
		},
	}).BuildAtom()
}

func buildPktHeadAtom(header []byte, pos uint32) *pcp.Atom {
	return (&pcp.ChanPktData{
		Type: pcp.NewID4("head"),
		Pos:  pos,
		Data: header,
	}).BuildAtom()
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// isSiteLocal reports whether the IPv4 address is in a private (site-local)
// range: 10/8, 172.16/12, 192.168/16, or 169.254/16 (link-local).
func isSiteLocal(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	if ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	return false
}

func ipToUint32(addr net.Addr) uint32 {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0
	}
	v, _ := pcp.IPv4ToUint32(tcp.IP)
	return v
}

const pingTimeout = 2 * time.Second

// pingHost performs a firewall reachability check by connecting to the remote
// host on the specified port, sending pcp\n + helo, and checking if the oleh
// session ID matches the expected peerID.
func pingHost(remoteIP net.IP, port uint16, peerID, mySessionID pcp.GnuID) bool {
	addr := net.JoinHostPort(remoteIP.String(), strconv.Itoa(int(port)))
	conn, err := net.DialTimeout("tcp", addr, pingTimeout)
	if err != nil {
		slog.Debug("ping: dial failed", "addr", addr, "err", err)
		return false
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(pingTimeout))

	// Send pcp\n magic: tag "pcp\n" + size 4 (LE) + version 1 (LE).
	var magic [12]byte
	copy(magic[0:4], "pcp\n")
	magic[4] = 4 // size LE
	magic[8] = 1 // version LE
	if _, err := conn.Write(magic[:]); err != nil {
		return false
	}

	// Send helo with our session ID.
	helo := (&pcp.HeloPacket{
		SessionID: mySessionID,
	}).BuildHeloAtom()
	if err := helo.Write(conn); err != nil {
		return false
	}

	// Read oleh.
	br := bufio.NewReader(conn)
	olehAtom, err := pcp.ReadAtom(br)
	if err != nil || olehAtom.Tag != pcp.PCPOleh {
		return false
	}
	oleh, err := pcp.ParseHeloPacket(olehAtom)
	if err != nil {
		return false
	}
	sid := oleh.SessionID
	if sid == peerID {
		slog.Debug("ping: succeeded", "addr", addr)
		return true
	}
	slog.Debug("ping: session ID mismatch", "addr", addr)
	return false
}
