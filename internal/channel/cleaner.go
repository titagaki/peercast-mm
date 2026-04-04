package channel

import (
	"log/slog"
	"sync"
	"time"

	"github.com/titagaki/peercast-pcp/pcp"
)

const cleanerInterval = 5 * time.Second

// Cleaner periodically removes idle relay channels from the Manager.
// Broadcast channels are never auto-removed.
//
// A relay channel is considered idle when it has no listeners and no relays.
// Once a channel becomes idle, the cleaner waits for InactiveLimit before
// removing it. If the channel gains a listener or relay before the limit,
// the timer resets.
type Cleaner struct {
	mgr           *Manager
	inactiveLimit time.Duration

	mu       sync.Mutex
	inactive map[pcp.GnuID]time.Time // channelID → first-seen-idle time

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewCleaner creates a Cleaner that removes idle relay channels after
// inactiveLimit has elapsed.
func NewCleaner(mgr *Manager, inactiveLimit time.Duration) *Cleaner {
	return &Cleaner{
		mgr:           mgr,
		inactiveLimit: inactiveLimit,
		inactive:      make(map[pcp.GnuID]time.Time),
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
}

// Run starts the periodic cleanup loop. It blocks until Stop is called.
func (c *Cleaner) Run() {
	defer close(c.doneCh)
	ticker := time.NewTicker(cleanerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.cleanup()
		}
	}
}

// Stop signals the cleaner to shut down and waits for it to exit.
func (c *Cleaner) Stop() {
	close(c.stopCh)
	<-c.doneCh
}

func (c *Cleaner) cleanup() {
	if c.inactiveLimit <= 0 {
		return
	}

	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, ch := range c.mgr.List() {
		if ch.IsBroadcasting() {
			continue
		}

		id := ch.ID
		isIdle := ch.NumListeners() == 0 && ch.NumRelays() == 0

		if isIdle {
			firstSeen, tracked := c.inactive[id]
			if !tracked {
				c.inactive[id] = now
			} else if now.Sub(firstSeen) > c.inactiveLimit {
				slog.Info("cleaner: removing idle channel", "channel", id)
				c.mgr.Stop(id)
				delete(c.inactive, id)
			}
		} else {
			delete(c.inactive, id)
		}
	}

	// Prune entries for channels that no longer exist.
	for id := range c.inactive {
		if _, ok := c.mgr.GetByID(id); !ok {
			delete(c.inactive, id)
		}
	}
}
