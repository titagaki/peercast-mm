package channel

import (
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

// OutputStreamType distinguishes PCP relay streams from HTTP direct streams.
type OutputStreamType int

const (
	OutputStreamPCP  OutputStreamType = iota // PCPOutputStream (downstream relay node)
	OutputStreamHTTP                         // HTTPOutputStream (media player)
)

// OutputStream is implemented by PCPOutputStream and HTTPOutputStream.
type OutputStream interface {
	// NotifyHeader is called when the stream header changes.
	NotifyHeader()
	// NotifyInfo is called when ChannelInfo changes.
	NotifyInfo()
	// NotifyTrack is called when TrackInfo changes.
	NotifyTrack()
	// Close terminates the output stream.
	Close()
	// Type returns whether this is a PCP relay or HTTP direct stream.
	Type() OutputStreamType
	// ID returns the unique connection identifier assigned by the Listener.
	ID() int
	// RemoteAddr returns the remote address string (e.g. "203.0.113.1:7144").
	RemoteAddr() string
	// SendRate returns bytes sent per second (last full second).
	SendRate() int64
}

// BcstForwarder is implemented by PCP output streams that participate in
// bcst atom forwarding. HTTP streams do not implement this interface.
type BcstForwarder interface {
	// SendBcst enqueues a bcst atom for forwarding to downstream.
	SendBcst(atom *pcp.Atom)
	// PeerID returns the session ID of the remote peer.
	PeerID() pcp.GnuID
}

// RelayEvictable is implemented by PCP output streams to report whether they
// are candidates for eviction when relay slots are full.
type RelayEvictable interface {
	// IsFirewalled reports whether the remote peer has no open port.
	IsFirewalled() bool
}

// ConnectionInfo is a snapshot of an active output connection.
type ConnectionInfo struct {
	ID         int
	Type       OutputStreamType
	RemoteAddr string
	SendRate   int64
}

// Channel is the central data structure for an active broadcast.
type Channel struct {
	ID        pcp.GnuID
	buffer    *ContentBuffer
	StartTime time.Time

	mu             sync.RWMutex
	broadcastID    pcp.GnuID
	isBroadcasting bool   // true for RTMP-sourced channels, false for relay channels
	source         string // display source (e.g. upstream addr for relay)
	upstreamAddr   string // upstream host:port for relay channels
	info           ChannelInfo
	track          TrackInfo
	outputs        []OutputStream
	numListeners   int // HTTPOutputStream の数
	numRelays      int // PCPOutputStream の数

	// Upstream node info (set by relay client on connect).
	upstreamSessionID pcp.GnuID
	upstreamIP        uint32
	upstreamPort      uint16

	// knownHosts is a bounded cache of Host atoms observed via bcst forwarding.
	// Used by SelectSourceHosts to hand out alternative relay candidates when
	// this node has no free relay slots (PeerCastStation 互換: Channel.Nodes).
	knownHosts []*pcp.Atom
}

const maxKnownHosts = 32

// New creates a new Channel. bufSize sets the ContentBuffer ring buffer size;
// if <= 0, DefaultContentBufferSize is used.
func New(id, broadcastID pcp.GnuID, bufSize int) *Channel {
	return &Channel{
		ID:          id,
		broadcastID: broadcastID,
		buffer:      NewContentBuffer(bufSize),
		StartTime:   time.Now(),
	}
}

// BroadcastID returns the broadcast ID.
func (c *Channel) BroadcastID() pcp.GnuID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.broadcastID
}

// SetBroadcastID updates the broadcast ID in a thread-safe manner.
func (c *Channel) SetBroadcastID(id pcp.GnuID) {
	c.mu.Lock()
	c.broadcastID = id
	c.mu.Unlock()
}

// IsBroadcasting reports whether this channel is sourced from a local RTMP push.
func (c *Channel) IsBroadcasting() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isBroadcasting
}

// Source returns the display source string (e.g. RTMP URI or upstream address).
func (c *Channel) Source() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.source
}

// SetSource sets the display source string.
func (c *Channel) SetSource(s string) {
	c.mu.Lock()
	c.source = s
	c.mu.Unlock()
}

// UpstreamAddr returns the upstream host:port for relay channels.
func (c *Channel) UpstreamAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.upstreamAddr
}

// SetUpstreamAddr sets the upstream address for relay channels.
func (c *Channel) SetUpstreamAddr(addr string) {
	c.mu.Lock()
	c.upstreamAddr = addr
	c.mu.Unlock()
}

// UpstreamNodeInfo returns the upstream node's session ID, IP, and port.
func (c *Channel) UpstreamNodeInfo() (sessionID pcp.GnuID, ip uint32, port uint16) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.upstreamSessionID, c.upstreamIP, c.upstreamPort
}

// SetUpstreamNodeInfo records the upstream node's session ID, IP, and port.
func (c *Channel) SetUpstreamNodeInfo(sessionID pcp.GnuID, ip uint32, port uint16) {
	c.mu.Lock()
	c.upstreamSessionID = sessionID
	c.upstreamIP = ip
	c.upstreamPort = port
	c.mu.Unlock()
}

// Info returns the current ChannelInfo.
func (c *Channel) Info() ChannelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.info
}

// Track returns the current TrackInfo.
func (c *Channel) Track() TrackInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.track
}

// SetInfo updates ChannelInfo and notifies outputs.
func (c *Channel) SetInfo(info ChannelInfo) {
	c.mu.Lock()
	c.info = info
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.Unlock()
	for _, o := range outputs {
		o.NotifyInfo()
	}
}

// SetTrack updates TrackInfo and notifies outputs.
func (c *Channel) SetTrack(track TrackInfo) {
	c.mu.Lock()
	c.track = track
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.Unlock()
	for _, o := range outputs {
		o.NotifyTrack()
	}
}

// SetHeader updates the stream header and notifies outputs. If the new header
// is identical to the current one (same bytes and pos) this is a no-op and
// outputs are not notified, avoiding spurious ring-buffer resets and header
// re-sends when upstreams resend an unchanged head packet periodically.
func (c *Channel) SetHeader(data []byte, pos uint32) {
	if !c.buffer.SetHeader(data, pos) {
		return
	}
	c.mu.RLock()
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.RUnlock()
	for _, o := range outputs {
		o.NotifyHeader()
	}
}

// Write appends a data packet to the buffer.
func (c *Channel) Write(data []byte, pos uint32, contFlags byte) {
	c.buffer.Write(data, pos, contFlags)
}

// HasData reports whether the buffer contains at least one packet.
func (c *Channel) HasData() bool {
	return c.buffer.HasData()
}

// Header returns the current stream header and its position.
func (c *Channel) Header() ([]byte, uint32) {
	return c.buffer.Header()
}

// Signal returns a channel that will be closed when new data is written.
func (c *Channel) Signal() <-chan struct{} {
	return c.buffer.Signal()
}

// Since returns all packets at or after the given stream position.
func (c *Channel) Since(pos uint32) []Content {
	return c.buffer.Since(pos)
}

// PacketsAfter returns all buffered packets strictly newer than ref, ordered
// by (Timestamp, Pos). Used by HTTPOutputStream.
func (c *Channel) PacketsAfter(ref Content) []Content {
	return c.buffer.PacketsAfter(ref)
}

// ContentPosition returns the byte position just past the newest content.
func (c *Channel) ContentPosition() uint32 {
	return c.buffer.ContentPosition()
}

// OldestPos returns the stream position of the oldest buffered packet.
func (c *Channel) OldestPos() uint32 {
	return c.buffer.OldestPos()
}

// NewestPos returns the stream position of the newest buffered packet.
func (c *Channel) NewestPos() uint32 {
	return c.buffer.NewestPos()
}

// AddOutput registers an output stream and updates the appropriate counter.
func (c *Channel) AddOutput(o OutputStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outputs = append(c.outputs, o)
	switch o.Type() {
	case OutputStreamPCP:
		c.numRelays++
	case OutputStreamHTTP:
		c.numListeners++
	}
}

// TryAddOutput registers an output stream only if the relevant limit has not been reached.
// maxRelays and maxListeners of 0 mean unlimited.
// Returns false if the limit is already reached; the caller must close the connection.
func (c *Channel) TryAddOutput(o OutputStream, maxRelays, maxListeners int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch o.Type() {
	case OutputStreamPCP:
		if maxRelays > 0 && c.numRelays >= maxRelays {
			return false
		}
		c.numRelays++
	case OutputStreamHTTP:
		if maxListeners > 0 && c.numListeners >= maxListeners {
			return false
		}
		c.numListeners++
	}
	c.outputs = append(c.outputs, o)
	return true
}

// MakeRelayable tries to free a relay slot by evicting a firewalled downstream
// node. Returns true if a slot was freed or was already available.
// PeerCastStation 互換: firewalled なノードを切断して枠を空ける。
func (c *Channel) MakeRelayable(maxRelays int) bool {
	if maxRelays <= 0 {
		return true
	}
	c.mu.RLock()
	if c.numRelays < maxRelays {
		c.mu.RUnlock()
		return true
	}
	// Find a firewalled relay to evict.
	var victim OutputStream
	for _, o := range c.outputs {
		if o.Type() != OutputStreamPCP {
			continue
		}
		if ev, ok := o.(RelayEvictable); ok && ev.IsFirewalled() {
			victim = o
			break
		}
	}
	c.mu.RUnlock()
	if victim == nil {
		return false
	}
	victim.Close()
	return true
}

// IsRelayFull reports whether the relay limit has been reached.
// Returns false when maxRelays is 0 (unlimited).
func (c *Channel) IsRelayFull(maxRelays int) bool {
	if maxRelays <= 0 {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.numRelays >= maxRelays
}

// IsDirectFull reports whether the direct listener limit has been reached.
// Returns false when maxListeners is 0 (unlimited).
func (c *Channel) IsDirectFull(maxListeners int) bool {
	if maxListeners <= 0 {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.numListeners >= maxListeners
}

// RemoveOutput unregisters an output stream and updates the appropriate counter.
func (c *Channel) RemoveOutput(o OutputStream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, out := range c.outputs {
		if out == o {
			c.outputs = append(c.outputs[:i], c.outputs[i+1:]...)
			switch o.Type() {
			case OutputStreamPCP:
				c.numRelays--
			case OutputStreamHTTP:
				c.numListeners--
			}
			return
		}
	}
}

// NumListeners returns the number of active HTTPOutputStream connections.
func (c *Channel) NumListeners() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.numListeners
}

// NumRelays returns the number of active PCPOutputStream connections.
func (c *Channel) NumRelays() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.numRelays
}

// CloseAll closes all registered output streams.
func (c *Channel) CloseAll() {
	c.mu.Lock()
	outputs := append([]OutputStream(nil), c.outputs...)
	c.outputs = nil
	c.numListeners = 0
	c.numRelays = 0
	c.mu.Unlock()
	for _, o := range outputs {
		o.Close()
	}
}

// UptimeSeconds returns the number of seconds since the channel started.
func (c *Channel) UptimeSeconds() uint32 {
	return uint32(time.Since(c.StartTime).Seconds())
}

// AddKnownHost records a Host atom observed via bcst forwarding, deduped by
// the host's session ID. Older entries are evicted when the cache is full.
func (c *Channel) AddKnownHost(host *pcp.Atom) {
	if host == nil {
		return
	}
	sidAtom := host.FindChild(pcp.PCPHostID)
	if sidAtom == nil {
		return
	}
	sid, err := sidAtom.GetID()
	if err != nil {
		return
	}
	var zero pcp.GnuID
	if sid == zero || sid == c.sessionIDForKnownHosts() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Replace existing entry for the same sid.
	for i, h := range c.knownHosts {
		if existing := h.FindChild(pcp.PCPHostID); existing != nil {
			if id, err := existing.GetID(); err == nil && id == sid {
				c.knownHosts[i] = host
				return
			}
		}
	}
	if len(c.knownHosts) >= maxKnownHosts {
		c.knownHosts = c.knownHosts[1:]
	}
	c.knownHosts = append(c.knownHosts, host)
}

// sessionIDForKnownHosts returns a zero id; the channel doesn't know its own
// session ID directly, so dedup against self is handled by callers.
func (c *Channel) sessionIDForKnownHosts() pcp.GnuID { return pcp.GnuID{} }

// SelectSourceHosts returns up to max recently observed Host atoms. Used by
// PCPOutputStream when the relay is full to hand out alternative nodes
// (PeerCastStation 互換: SelectSourceHosts)。
func (c *Channel) SelectSourceHosts(max int) []*pcp.Atom {
	if max <= 0 {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := len(c.knownHosts)
	if n > max {
		n = max
	}
	if n == 0 {
		return nil
	}
	// Return newest first.
	out := make([]*pcp.Atom, 0, n)
	for i := len(c.knownHosts) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, c.knownHosts[i])
	}
	return out
}

// Broadcast forwards a bcst atom to all PCP output streams except the sender
// and any other connections from the same peer (same session ID).
func (c *Channel) Broadcast(from OutputStream, atom *pcp.Atom) {
	var fromPeerID pcp.GnuID
	if bf, ok := from.(BcstForwarder); ok {
		fromPeerID = bf.PeerID()
	}
	c.mu.RLock()
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.RUnlock()
	for _, o := range outputs {
		if o == from || o.Type() != OutputStreamPCP {
			continue
		}
		bf, ok := o.(BcstForwarder)
		if !ok {
			continue
		}
		if bf.PeerID() == fromPeerID {
			continue // 同一ピアの別接続には転送しない（ループ防止）
		}
		bf.SendBcst(atom)
	}
}

// Connections returns a snapshot of all active output connections.
func (c *Channel) Connections() []ConnectionInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]ConnectionInfo, len(c.outputs))
	for i, o := range c.outputs {
		result[i] = ConnectionInfo{
			ID:         o.ID(),
			Type:       o.Type(),
			RemoteAddr: o.RemoteAddr(),
			SendRate:   o.SendRate(),
		}
	}
	return result
}

// CloseConnection closes the output connection with the given ID.
// Returns true if found and closed, false if no connection with that ID exists.
func (c *Channel) CloseConnection(id int) bool {
	c.mu.RLock()
	var target OutputStream
	for _, o := range c.outputs {
		if o.ID() == id {
			target = o
			break
		}
	}
	c.mu.RUnlock()
	if target == nil {
		return false
	}
	target.Close()
	return true
}
