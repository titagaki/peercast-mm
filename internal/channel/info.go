package channel

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

// TrackInfo holds current track metadata.
type TrackInfo struct {
	Title   string
	Creator string
	URL     string
	Album   string
}
