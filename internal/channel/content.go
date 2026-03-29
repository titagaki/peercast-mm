package channel

import "sync"

const ContentBufferSize = 64

// Content is a single stream data packet.
type Content struct {
	Pos  uint32
	Data []byte
	Cont bool // true = continuation (not a keyframe)
}

// ContentBuffer holds the stream header and a fixed-size ring buffer of data packets.
type ContentBuffer struct {
	mu      sync.RWMutex
	header  []byte
	headerPos uint32
	packets [ContentBufferSize]Content
	count   int // total packets written (used to compute ring positions)
}

// SetHeader updates the stream header.
func (b *ContentBuffer) SetHeader(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	if len(b.packets) > 0 && b.count > 0 {
		b.headerPos = b.packets[(b.count-1)%ContentBufferSize].Pos + uint32(len(b.packets[(b.count-1)%ContentBufferSize].Data))
	}
	b.header = cp
}

// Write appends a data packet.
func (b *ContentBuffer) Write(data []byte, pos uint32, cont bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	b.packets[b.count%ContentBufferSize] = Content{Pos: pos, Data: cp, Cont: cont}
	b.count++
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
	if b.count <= ContentBufferSize {
		return b.packets[0].Pos
	}
	return b.packets[b.count%ContentBufferSize].Pos
}

// NewestPos returns the stream position of the newest buffered packet.
// Returns 0 if the buffer is empty.
func (b *ContentBuffer) NewestPos() uint32 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.count == 0 {
		return 0
	}
	return b.packets[(b.count-1)%ContentBufferSize].Pos
}

// Since returns all packets at or after the given stream position.
// If pos is older than the oldest buffered packet, returns from the oldest.
// Returns the first keyframe (Cont=false) onwards.
func (b *ContentBuffer) Since(pos uint32) []Content {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.count == 0 {
		return nil
	}

	// Determine the range of valid indices in the ring buffer.
	start := 0
	end := b.count
	if b.count > ContentBufferSize {
		start = b.count - ContentBufferSize
	}

	// Find the first index >= pos.
	firstIdx := -1
	for i := start; i < end; i++ {
		p := b.packets[i%ContentBufferSize]
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
		result = append(result, b.packets[i%ContentBufferSize])
	}
	return result
}

// HasData returns true if the buffer contains at least one packet.
func (b *ContentBuffer) HasData() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.count > 0
}
