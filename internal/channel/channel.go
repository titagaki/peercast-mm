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
	// SendBcst enqueues a bcst atom for forwarding to downstream.
	// Only meaningful for PCP output streams; HTTP streams ignore this.
	SendBcst(atom *pcp.Atom)
	// PeerID returns the session ID of the remote peer.
	// Used by Broadcast to avoid forwarding back to the same peer on multiple connections.
	PeerID() pcp.GnuID
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
	Buffer    *ContentBuffer
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
}

// New creates a new Channel. bufSize sets the ContentBuffer ring buffer size;
// if <= 0, DefaultContentBufferSize is used.
func New(id, broadcastID pcp.GnuID, bufSize int) *Channel {
	return &Channel{
		ID:          id,
		broadcastID: broadcastID,
		Buffer:      NewContentBuffer(bufSize),
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

// SetHeader updates the stream header and notifies outputs.
func (c *Channel) SetHeader(data []byte) {
	c.Buffer.SetHeader(data)
	c.mu.RLock()
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.RUnlock()
	for _, o := range outputs {
		o.NotifyHeader()
	}
}

// Write appends a data packet to the buffer.
func (c *Channel) Write(data []byte, pos uint32, contFlags byte) {
	c.Buffer.Write(data, pos, contFlags)
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

// Broadcast forwards a bcst atom to all PCP output streams except the sender
// and any other connections from the same peer (same session ID).
func (c *Channel) Broadcast(from OutputStream, atom *pcp.Atom) {
	fromPeerID := from.PeerID()
	c.mu.RLock()
	outputs := append([]OutputStream(nil), c.outputs...)
	c.mu.RUnlock()
	for _, o := range outputs {
		if o == from || o.Type() != OutputStreamPCP {
			continue
		}
		if o.PeerID() == fromPeerID {
			continue // 同一ピアの別接続には転送しない（ループ防止）
		}
		o.SendBcst(atom)
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
