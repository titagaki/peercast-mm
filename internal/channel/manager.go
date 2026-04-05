package channel

import (
	"fmt"
	"sync"

	"github.com/titagaki/peercast-pcp/pcp"

	"github.com/titagaki/peercast-mi/internal/id"
)

// RelayHandle is implemented by relay.Client. Using an interface here avoids
// an import cycle between the channel and relay packages.
type RelayHandle interface {
	Stop()
	SetGlobalIP(ip uint32)
}

// Manager manages stream keys and active broadcast channels.
//
// Stream keys are long-lived: issuing a key and stopping a channel that uses
// it are independent operations. A key remains valid until revoked.
//
// Lifecycle:
//
//	IssueStreamKey(accountName, streamKey) → persisted to cache
//	Broadcast(streamKey, info, track) → *Channel + channelID
//	Stop(channelID) → channel removed, streamKey still valid
//	RevokeStreamKey(accountName) → key invalidated, active channels NOT stopped
type Manager struct {
	broadcastID pcp.GnuID
	keys        *StreamKeyStore

	// ContentBufferSeconds is the duration (in seconds) the ring buffer
	// should cover for new channels. Packet count is computed from bitrate.
	// 0 means use DefaultContentBufferSeconds.
	ContentBufferSeconds float64

	mu            sync.RWMutex
	byID          map[pcp.GnuID]*Channel
	byStreamKey   map[string]*Channel       // active channels only
	streamKeyByID map[pcp.GnuID]string      // reverse map for status display
	relays        map[pcp.GnuID]RelayHandle // relay clients keyed by channel ID
}

// NewManager creates a new Manager. broadcastID is the node-level identifier
// used as the seed for deterministic channel ID generation.
func NewManager(broadcastID pcp.GnuID) *Manager {
	return &Manager{
		broadcastID:   broadcastID,
		keys:          NewStreamKeyStore(),
		byID:          make(map[pcp.GnuID]*Channel),
		byStreamKey:   make(map[string]*Channel),
		streamKeyByID: make(map[pcp.GnuID]string),
		relays:        make(map[pcp.GnuID]RelayHandle),
	}
}

// SetCachePath sets the path to the stream key cache file.
// Call LoadCache after setting to populate from disk.
func (m *Manager) SetCachePath(path string) { m.keys.SetCachePath(path) }

// LoadCache reads the cache file and populates the in-memory stream key store.
func (m *Manager) LoadCache() error { return m.keys.LoadCache() }

// IssueStreamKey registers an accountName → streamKey mapping.
func (m *Manager) IssueStreamKey(accountName, streamKey string) error {
	return m.keys.IssueStreamKey(accountName, streamKey)
}

// RevokeStreamKey removes the stream key for the given account.
func (m *Manager) RevokeStreamKey(accountName string) bool {
	return m.keys.RevokeStreamKey(accountName)
}

// IsIssuedKey reports whether the given stream key has been issued.
func (m *Manager) IsIssuedKey(key string) bool { return m.keys.IsIssuedKey(key) }

// ListStreamKeys returns all issued stream keys, sorted by account name.
func (m *Manager) ListStreamKeys() []StreamKeyEntry { return m.keys.List() }

// Broadcast starts a new channel for the given stream key.
//
// Returns an error if:
//   - the stream key has not been issued
//   - the stream key already has an active channel (call Stop first)
//
// The channel ID is deterministically derived from the inputs, so calling
// Broadcast again with identical arguments after stopping yields the same ID.
func (m *Manager) Broadcast(streamKey string, info ChannelInfo, track TrackInfo) (*Channel, error) {
	if !m.keys.IsIssuedKey(streamKey) {
		return nil, fmt.Errorf("stream key not issued")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
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
	channels := m.List()
	total := 0
	for _, ch := range channels {
		total += ch.NumRelays()
	}
	return total
}

// TotalSendRate returns the total send rate (bytes/sec) across all channels.
func (m *Manager) TotalSendRate() int64 {
	channels := m.List()
	var total int64
	for _, ch := range channels {
		for _, c := range ch.Connections() {
			total += c.SendRate
		}
	}
	return total
}

// SetGlobalIPForRelays propagates the global IP to all active relay clients.
func (m *Manager) SetGlobalIPForRelays(ip uint32) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.relays {
		r.SetGlobalIP(ip)
	}
}

// channelIDForBroadcast deterministically generates a channel ID from the
// broadcast node ID, stream key, and channel metadata.
func channelIDForBroadcast(broadcastID pcp.GnuID, streamKey, name, genre string, bitrate uint32) pcp.GnuID {
	// Embed the stream key into the name using a null-byte separator to avoid
	// collisions between different (name, streamKey) combinations.
	return id.ChannelID(broadcastID, name+"\x00"+streamKey, genre, bitrate)
}
