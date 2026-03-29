package id

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha512"

	"github.com/titagaki/peercast-pcp/pcp"
)

// NewRandom generates a random 16-byte GnuID.
func NewRandom() pcp.GnuID {
	var id pcp.GnuID
	if _, err := rand.Read(id[:]); err != nil {
		panic("id: failed to read random bytes: " + err.Error())
	}
	return id
}

// ChannelID deterministically generates a channel ID from the given fields.
//
//	input = BroadcastID || NetworkType || ChannelName || Genre || SourceURL
//	ChannelID = MD5(SHA512(input))[:16]
func ChannelID(broadcastID pcp.GnuID, channelName, genre, sourceURL string) pcp.GnuID {
	const networkType = "ipv4"

	h := sha512.New()
	h.Write(broadcastID[:])
	h.Write([]byte(networkType))
	h.Write([]byte(channelName))
	h.Write([]byte(genre))
	h.Write([]byte(sourceURL))
	sha := h.Sum(nil)

	sum := md5.Sum(sha)
	var id pcp.GnuID
	copy(id[:], sum[:])
	return id
}
