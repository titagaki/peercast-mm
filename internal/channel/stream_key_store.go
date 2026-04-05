package channel

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
)

// StreamKeyStore manages the mapping between account names and stream keys.
// Stream keys are long-lived: issuing a key and stopping a channel that uses
// it are independent operations. A key remains valid until revoked.
type StreamKeyStore struct {
	cachePath string

	mu         sync.RWMutex
	accounts   map[string]string // accountName → streamKey
	streamKeys map[string]string // streamKey → accountName (for O(1) lookup)
}

// NewStreamKeyStore creates a new StreamKeyStore.
func NewStreamKeyStore() *StreamKeyStore {
	return &StreamKeyStore{
		accounts:   make(map[string]string),
		streamKeys: make(map[string]string),
	}
}

// SetCachePath sets the path to the stream key cache file.
// Call LoadCache after setting to populate from disk.
func (s *StreamKeyStore) SetCachePath(path string) {
	s.cachePath = path
}

// LoadCache reads the cache file and populates the in-memory stream key store.
// If the file does not exist, it is silently ignored.
func (s *StreamKeyStore) LoadCache() error {
	if s.cachePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.cachePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stream key cache: read %s: %w", s.cachePath, err)
	}
	var cache struct {
		Accounts map[string]string `json:"accounts"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		return fmt.Errorf("stream key cache: parse %s: %w", s.cachePath, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, key := range cache.Accounts {
		s.accounts[name] = key
		s.streamKeys[key] = name
	}
	return nil
}

func (s *StreamKeyStore) saveCache() error {
	if s.cachePath == "" {
		return nil
	}
	s.mu.RLock()
	accounts := make(map[string]string, len(s.accounts))
	for name, key := range s.accounts {
		accounts[name] = key
	}
	s.mu.RUnlock()

	data, err := json.Marshal(struct {
		Accounts map[string]string `json:"accounts"`
	}{Accounts: accounts})
	if err != nil {
		return err
	}
	// Write atomically via temp file + rename.
	tmp := s.cachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("stream key cache: write %s: %w", s.cachePath, err)
	}
	return os.Rename(tmp, s.cachePath)
}

// IssueStreamKey registers an accountName → streamKey mapping.
// If accountName already exists, the old stream key is replaced.
// The mapping is persisted to the cache file.
func (s *StreamKeyStore) IssueStreamKey(accountName, streamKey string) error {
	s.mu.Lock()
	if oldKey, ok := s.accounts[accountName]; ok {
		delete(s.streamKeys, oldKey)
	}
	s.accounts[accountName] = streamKey
	s.streamKeys[streamKey] = accountName
	s.mu.Unlock()
	return s.saveCache()
}

// RevokeStreamKey removes the stream key for the given account.
// Returns false if the account was not found.
func (s *StreamKeyStore) RevokeStreamKey(accountName string) bool {
	s.mu.Lock()
	key, ok := s.accounts[accountName]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.accounts, accountName)
	delete(s.streamKeys, key)
	s.mu.Unlock()
	if err := s.saveCache(); err != nil {
		slog.Warn("stream key cache: save failed on revoke", "err", err)
	}
	return true
}

// IsIssuedKey reports whether the given stream key has been issued.
func (s *StreamKeyStore) IsIssuedKey(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.streamKeys[key]
	return ok
}

// StreamKeyEntry is a single accountName → streamKey mapping.
type StreamKeyEntry struct {
	AccountName string
	StreamKey   string
}

// List returns all issued stream keys, sorted by account name.
func (s *StreamKeyStore) List() []StreamKeyEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make([]StreamKeyEntry, 0, len(s.accounts))
	for name, key := range s.accounts {
		entries = append(entries, StreamKeyEntry{AccountName: name, StreamKey: key})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].AccountName < entries[j].AccountName
	})
	return entries
}
