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
}

// Channel is the central data structure for an active broadcast.
type Channel struct {
	ID          pcp.GnuID
	BroadcastID pcp.GnuID
	Buffer      *ContentBuffer
	StartTime   time.Time

	mu           sync.RWMutex
	info         ChannelInfo
	track        TrackInfo
	outputs      []OutputStream
	numListeners int // HTTPOutputStream の数
	numRelays    int // PCPOutputStream の数
}

// New creates a new Channel.
func New(id, broadcastID pcp.GnuID) *Channel {
	return &Channel{
		ID:          id,
		BroadcastID: broadcastID,
		Buffer:      &ContentBuffer{},
		StartTime:   time.Now(),
	}
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
func (c *Channel) Write(data []byte, pos uint32, cont bool) {
	c.Buffer.Write(data, pos, cont)
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
