package channel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mm/internal/id"
)

// RelayHandle is implemented by relay.Client. Using an interface here avoids
// an import cycle between the channel and relay packages.
type RelayHandle interface {
	Stop()
}

// Manager manages stream keys and active broadcast channels.
//
// Stream keys are long-lived: issuing a key and stopping a channel that uses
// it are independent operations. A key remains valid until the process exits.
//
// Lifecycle:
//
//	IssueStreamKey() → streamKey (persists across channel stops)
//	Broadcast(streamKey, info, track) → *Channel + channelID
//	Stop(channelID) → channel removed, streamKey still valid
//	Broadcast(same args) → same channelID (deterministic)
type Manager struct {
	broadcastID pcp.GnuID

	// ContentBufferSeconds is the duration (in seconds) the ring buffer
	// should cover for new channels. Packet count is computed from bitrate.
	// 0 means use DefaultContentBufferSeconds.
	ContentBufferSeconds float64

	mu            sync.RWMutex
	streamKeys    map[string]struct{}   // issued stream keys
	byID          map[pcp.GnuID]*Channel
	byStreamKey   map[string]*Channel   // active channels only
	streamKeyByID map[pcp.GnuID]string  // reverse map for status display
	relays        map[pcp.GnuID]RelayHandle // relay clients keyed by channel ID
}

// NewManager creates a new Manager. broadcastID is the node-level identifier
// used as the seed for deterministic channel ID generation.
func NewManager(broadcastID pcp.GnuID) *Manager {
	return &Manager{
		broadcastID:   broadcastID,
		streamKeys:    make(map[string]struct{}),
		byID:          make(map[pcp.GnuID]*Channel),
		byStreamKey:   make(map[string]*Channel),
		streamKeyByID: make(map[pcp.GnuID]string),
		relays:        make(map[pcp.GnuID]RelayHandle),
	}
}

// IssueStreamKey generates and registers a new stream key.
// The key persists beyond any individual channel's lifetime.
func (m *Manager) IssueStreamKey() string {
	key := newStreamKey()
	m.mu.Lock()
	m.streamKeys[key] = struct{}{}
	m.mu.Unlock()
	return key
}

// IsIssuedKey reports whether the given stream key has been issued.
func (m *Manager) IsIssuedKey(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.streamKeys[key]
	return ok
}

// Broadcast starts a new channel for the given stream key.
//
// Returns an error if:
//   - the stream key has not been issued
//   - the stream key already has an active channel (call Stop first)
//
// The channel ID is deterministically derived from the inputs, so calling
// Broadcast again with identical arguments after stopping yields the same ID.
func (m *Manager) Broadcast(streamKey string, info ChannelInfo, track TrackInfo) (*Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.streamKeys[streamKey]; !ok {
		return nil, fmt.Errorf("stream key not issued")
	}
	if _, ok := m.byStreamKey[streamKey]; ok {
		return nil, fmt.Errorf("stream key %q already has an active channel", streamKey)
	}
	channelID := channelIDForBroadcast(m.broadcastID, streamKey, info.Name, info.Genre, info.Bitrate)
	bufSize := ContentBufferSizeForBitrate(info.Bitrate, m.ContentBufferSeconds)
	ch := New(channelID, m.broadcastID, bufSize)
	// Set fields directly: ch is not yet visible to other goroutines.
	ch.isBroadcasting = true
	ch.info = info
	ch.track = track
	m.byID[channelID] = ch
	m.byStreamKey[streamKey] = ch
	m.streamKeyByID[channelID] = streamKey
	return ch, nil
}

// Stop closes and deregisters the channel with the given ID.
// The associated stream key remains valid for future broadcasts.
// Returns false if no active channel with that ID exists.
func (m *Manager) Stop(channelID pcp.GnuID) bool {
	m.mu.Lock()
	ch, ok := m.byID[channelID]
	if !ok {
		m.mu.Unlock()
		return false
	}
	key := m.streamKeyByID[channelID]
	delete(m.byStreamKey, key)
	delete(m.byID, channelID)
	delete(m.streamKeyByID, channelID)
	relay := m.relays[channelID]
	delete(m.relays, channelID)
	m.mu.Unlock()
	if relay != nil {
		relay.Stop()
	}
	ch.CloseAll()
	return true
}

// StopAll stops all active channels. Stream keys remain valid.
func (m *Manager) StopAll() {
	m.mu.Lock()
	channels := make([]*Channel, 0, len(m.byID))
	for _, ch := range m.byID {
		channels = append(channels, ch)
	}
	relays := make([]RelayHandle, 0, len(m.relays))
	for _, r := range m.relays {
		relays = append(relays, r)
	}
	m.byID = make(map[pcp.GnuID]*Channel)
	m.byStreamKey = make(map[string]*Channel)
	m.streamKeyByID = make(map[pcp.GnuID]string)
	m.relays = make(map[pcp.GnuID]RelayHandle)
	m.mu.Unlock()
	for _, r := range relays {
		r.Stop()
	}
	for _, ch := range channels {
		ch.CloseAll()
	}
}

// AddRelayChannel registers a channel that receives data from an upstream node
// via a relay client. The relay client must be started separately.
func (m *Manager) AddRelayChannel(ch *Channel, r RelayHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[ch.ID] = ch
	m.relays[ch.ID] = r
}

// GetByStreamKey returns the active channel for the given stream key, if any.
func (m *Manager) GetByStreamKey(key string) (*Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ch, ok := m.byStreamKey[key]
	return ch, ok
}

// GetByID returns the active channel with the given ID, if any.
func (m *Manager) GetByID(channelID pcp.GnuID) (*Channel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ch, ok := m.byID[channelID]
	return ch, ok
}

// StreamKeyByID returns the stream key associated with the given channel ID.
func (m *Manager) StreamKeyByID(channelID pcp.GnuID) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key, ok := m.streamKeyByID[channelID]
	return key, ok
}

// List returns a snapshot of all currently active channels.
func (m *Manager) List() []*Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	channels := make([]*Channel, 0, len(m.byID))
	for _, ch := range m.byID {
		channels = append(channels, ch)
	}
	return channels
}

// TotalRelays returns the total number of active PCP relay connections
// across all channels.
func (m *Manager) TotalRelays() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := 0
	for _, ch := range m.byID {
		total += ch.NumRelays()
	}
	return total
}

// TotalSendRate returns the total send rate (bytes/sec) across all channels.
func (m *Manager) TotalSendRate() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var total int64
	for _, ch := range m.byID {
		for _, c := range ch.Connections() {
			total += c.SendRate
		}
	}
	return total
}

// channelIDForBroadcast deterministically generates a channel ID from the
// broadcast node ID, stream key, and channel metadata.
func channelIDForBroadcast(broadcastID pcp.GnuID, streamKey, name, genre string, bitrate uint32) pcp.GnuID {
	// Embed the stream key into the name using a null-byte separator to avoid
	// collisions between different (name, streamKey) combinations.
	return id.ChannelID(broadcastID, name+"\x00"+streamKey, genre, bitrate)
}

// newStreamKey generates a random stream key with a "sk_" prefix.
func newStreamKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("channel: failed to generate stream key: " + err.Error())
	}
	return "sk_" + hex.EncodeToString(b)
}
