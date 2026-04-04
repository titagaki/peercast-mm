package relay

import (
	"math/rand/v2"
)

// selectSourceHost picks the best connectable host from the source node list,
// using PeerCastStation's scoring algorithm.
// Returns "" if no host is available.
func selectSourceHost(nodes []SourceNode, ignored *IgnoredNodeCollection, trackerAddr string) string {
	bestAddr := ""
	bestScore := -1.0

	for _, n := range nodes {
		if n.IsFirewalled {
			continue
		}

		// Determine preferred address.
		addr := n.GlobalAddr
		if n.LocalAddr != "" && isSiteLocal(n.LocalAddr) {
			addr = n.LocalAddr
		}
		if addr == "" {
			continue
		}
		if ignored.Contains(addr) {
			continue
		}

		// PeerCastStation scoring:
		//   (isSiteLocal ? 8000 : 0) + rand * (
		//     (isReceiving ? 4000 : 0) +
		//     (!isRelayFull ? 2000 : 0) +
		//     (max(10-hops, 0) * 100) +
		//     (relayCount * 10)
		//   )
		var score float64
		if isSiteLocal(addr) {
			score += 8000
		}

		var bonus float64
		if n.IsReceiving {
			bonus += 4000
		}
		if !n.IsRelayFull {
			bonus += 2000
		}
		hops := int(n.Hops)
		if hops < 10 {
			bonus += float64((10 - hops) * 100)
		}
		bonus += float64(n.RelayCount * 10)

		score += rand.Float64() * bonus

		if score > bestScore {
			bestScore = score
			bestAddr = addr
		}
	}

	if bestAddr != "" {
		return bestAddr
	}

	// Fall back to tracker if not ignored.
	if trackerAddr != "" && !ignored.Contains(trackerAddr) {
		return trackerAddr
	}
	return ""
}
