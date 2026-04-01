package version

import "fmt"

const Version = "0.0.1"

// PCPVersion is the PCP protocol version number used in helo/host atoms.
const PCPVersion = 1218

// PCPVersionVP is the VP extension version number used in host atoms.
const PCPVersionVP = 27

const AgentName = "PeerCast-MI/" + Version

// PCPClientMinVersion is the minimum PCP version required of connecting clients.
const PCPClientMinVersion = 1200

// ExPrefix is the 2-byte version extension prefix sent in PCP host atoms.
const ExPrefix = "MI"

// ExNumber converts a "major.minor.patch" version string to a 3-digit integer.
// e.g. "0.0.1" → 1, "1.2.3" → 123.
func ExNumber() uint16 {
	var major, minor, patch uint16
	fmt.Sscanf(Version, "%d.%d.%d", &major, &minor, &patch)
	return major*100 + minor*10 + patch
}
