package channel

import "sync"

const DefaultContentBufferSize = 64

// DefaultContentBufferSeconds is the default duration the ring buffer covers.
const DefaultContentBufferSeconds = 8.0

// pcpPacketSize is the approximate size of a single PCP data packet in bytes.
const pcpPacketSize = 15 * 1024

// ContentBufferSizeForBitrate computes the number of ring buffer packets
// needed to hold the given number of seconds at the specified bitrate.
// If bitrateKbps is 0 or seconds is <= 0, DefaultContentBufferSize is returned.
// The result is clamped to a minimum of DefaultContentBufferSize.
func ContentBufferSizeForBitrate(bitrateKbps uint32, seconds float64) int {
	if bitrateKbps == 0 || seconds <= 0 {
		return DefaultContentBufferSize
	}
	bytesPerSec := float64(bitrateKbps) * 1000 / 8
	totalBytes := bytesPerSec * seconds
	packets := int(totalBytes/pcpPacketSize) + 1
	if packets < DefaultContentBufferSize {
		packets = DefaultContentBufferSize
	}
	return packets
}

// Content is a single stream data packet.
type Content struct {
	Pos      uint32
	Data     []byte
	ContFlags byte // PeerCastStation 互換ビットフラグ (0x00=None, 0x01=Fragment, 0x02=InterFrame, 0x04=AudioFrame)
}

// ContentBuffer holds the stream header and a configurable-size ring buffer of data packets.
// It provides a Signal channel that is closed when new data arrives, allowing
// consumers to wait for data without polling.
type ContentBuffer struct {
	mu        sync.RWMutex
	header    []byte
	headerPos uint32
	packets   []Content
	count     int // total packets written (used to compute ring positions)

	sigOnce sync.Once
	sigMu   sync.Mutex
	sigCh   chan struct{} // closed on each Write; replaced with a new channel
}

// NewContentBuffer creates a ContentBuffer with the given ring buffer size.
// If size <= 0, DefaultContentBufferSize is used.
func NewContentBuffer(size int) *ContentBuffer {
	if size <= 0 {
		size = DefaultContentBufferSize
	}
	return &ContentBuffer{
		packets: make([]Content, size),
	}
}

func (b *ContentBuffer) initSig() {
	b.sigOnce.Do(func() {
		b.sigCh = make(chan struct{})
	})
}

// Signal returns a channel that will be closed when new data is written.
// Callers should obtain this channel before checking Since() to avoid races:
//
//	sigCh := buf.Signal()
//	packets := buf.Since(pos)
//	if len(packets) == 0 {
//	    <-sigCh // blocks until next Write
//	}
func (b *ContentBuffer) Signal() <-chan struct{} {
	b.initSig()
	b.sigMu.Lock()
	defer b.sigMu.Unlock()
	return b.sigCh
}

// notifyWrite closes the current signal channel and replaces it, waking all
// goroutines blocked on Signal().
func (b *ContentBuffer) notifyWrite() {
	b.initSig()
	b.sigMu.Lock()
	old := b.sigCh
	b.sigCh = make(chan struct{})
	b.sigMu.Unlock()
	close(old)
}

// SetHeader updates the stream header.
func (b *ContentBuffer) SetHeader(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	if len(b.packets) > 0 && b.count > 0 {
		size := len(b.packets)
		b.headerPos = b.packets[(b.count-1)%size].Pos + uint32(len(b.packets[(b.count-1)%size].Data))
	}
	b.header = cp
}

// Write appends a data packet and wakes all goroutines waiting on Signal().
func (b *ContentBuffer) Write(data []byte, pos uint32, contFlags byte) {
	b.mu.Lock()
	if b.packets == nil {
		b.packets = make([]Content, DefaultContentBufferSize)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	b.packets[b.count%len(b.packets)] = Content{Pos: pos, Data: cp, ContFlags: contFlags}
	b.count++
	b.mu.Unlock()
	b.notifyWrite()
}

// Header returns the current stream header and its position.
func (b *ContentBuffer) Header() ([]byte, uint32) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.header, b.headerPos
}

// OldestPos returns the stream position of the oldest buffered packet.
// Returns 0 if the buffer is empty.
func (b *ContentBuffer) OldestPos() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return 0
	}
	size := len(b.packets)
	if b.count <= size {
		return b.packets[0].Pos
	}
	return b.packets[b.count%size].Pos
}

// NewestPos returns the stream position of the newest buffered packet.
// Returns 0 if the buffer is empty.
func (b *ContentBuffer) NewestPos() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return 0
	}
	return b.packets[(b.count-1)%len(b.packets)].Pos
}

// Since returns all packets at or after the given stream position.
// If pos is older than the oldest buffered packet, returns from the oldest.
// Packets are returned as-is regardless of keyframe status; callers that need
// keyframe-aligned delivery (new connections) must apply their own filter.
func (b *ContentBuffer) Since(pos uint32) []Content {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 {
		return nil
	}

	// Determine the range of valid indices in the ring buffer.
	start := 0
	end := b.count
	size := len(b.packets)
	if b.count > size {
		start = b.count - size
	}

	// Find the first index >= pos.
	firstIdx := -1
	for i := start; i < end; i++ {
		p := b.packets[i%size]
		if p.Pos >= pos {
			firstIdx = i
			break
		}
	}
	if firstIdx < 0 {
		// All packets are older than pos; nothing new.
		return nil
	}

	result := make([]Content, 0, end-firstIdx)
	for i := firstIdx; i < end; i++ {
		result = append(result, b.packets[i%size])
	}
	return result
}

// HasData returns true if the buffer contains at least one packet.
func (b *ContentBuffer) HasData() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count > 0
}
