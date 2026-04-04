package channel

import "github.com/titagaki/peercast-pcp/pcp"

// ChannelInfo holds channel metadata.
type ChannelInfo struct {
	Name     string
	URL      string
	Desc     string
	Comment  string
	Genre    string
	Type     string // e.g. "FLV"
	MIMEType string // e.g. "video/x-flv"
	Ext      string // e.g. ".flv"
	Bitrate  uint32 // kbps
}

// ToPCP converts to the library's ChanInfo type.
func (ci ChannelInfo) ToPCP() pcp.ChanInfo {
	return pcp.ChanInfo{
		Name:       ci.Name,
		URL:        ci.URL,
		Desc:       ci.Desc,
		Comment:    ci.Comment,
		Genre:      ci.Genre,
		Type:       ci.Type,
		StreamType: ci.MIMEType,
		StreamExt:  ci.Ext,
		Bitrate:    ci.Bitrate,
	}
}

// ChannelInfoFromPCP converts the library's ChanInfo to a ChannelInfo.
func ChannelInfoFromPCP(ci pcp.ChanInfo) ChannelInfo {
	info := ChannelInfo{
		Name:     ci.Name,
		URL:      ci.URL,
		Desc:     ci.Desc,
		Comment:  ci.Comment,
		Genre:    ci.Genre,
		Type:     ci.Type,
		MIMEType: ci.StreamType,
		Ext:      ci.StreamExt,
		Bitrate:  ci.Bitrate,
	}
	// Infer MIME type and extension from content type when not set on the wire.
	if info.MIMEType == "" || info.Ext == "" {
		switch info.Type {
		case "FLV":
			if info.MIMEType == "" {
				info.MIMEType = "video/x-flv"
			}
			if info.Ext == "" {
				info.Ext = ".flv"
			}
		}
	}
	return info
}

// TrackInfo holds current track metadata.
type TrackInfo struct {
	Title   string
	Creator string
	URL     string
	Album   string
}

// ToPCP converts to the library's ChanTrack type.
func (ti TrackInfo) ToPCP() pcp.ChanTrack {
	return pcp.ChanTrack{
		Title:   ti.Title,
		Creator: ti.Creator,
		URL:     ti.URL,
		Album:   ti.Album,
	}
}

// TrackInfoFromPCP converts the library's ChanTrack to a TrackInfo.
func TrackInfoFromPCP(ct pcp.ChanTrack) TrackInfo {
	return TrackInfo{
		Title:   ct.Title,
		Creator: ct.Creator,
		URL:     ct.URL,
		Album:   ct.Album,
	}
}
