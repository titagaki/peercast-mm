package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type YP struct {
	Name string `toml:"name"`
	Addr string `toml:"addr"`
}

// HostPort returns the "host:port" string for use with pcp.Dial.
// Accepts either a plain "host:port" or a "pcp://host[:port]/" URL.
// The default port is 7144.
func (y *YP) HostPort() (string, error) {
	u, err := url.Parse(y.Addr)
	if err != nil || u.Scheme == "" {
		// Treat as plain host:port.
		return y.Addr, nil
	}
	if u.Scheme != "pcp" {
		return "", fmt.Errorf("unsupported scheme %q in yp addr %q", u.Scheme, y.Addr)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "7144"
	}
	return host + ":" + port, nil
}

type Config struct {
	RTMPPort     int    `toml:"rtmp_port"`
	PeercastPort int    `toml:"peercast_port"`
	LogLevel     string `toml:"log_level"`
	// MaxRelays はチャンネルに直接接続できる下流リレーノード数の上限。
	// 0 は無制限。
	MaxRelays int `toml:"max_relays"`
	// MaxListeners はチャンネルに直接接続できる HTTP 視聴者数の上限。
	// 0 は無制限。
	MaxListeners int `toml:"max_listeners"`
	// ContentBufferSeconds はコンテンツリングバッファが保持する秒数。
	// ビットレートからパケット数を自動計算する。0 はデフォルト (8秒) を使用。
	ContentBufferSeconds float64 `toml:"content_buffer_seconds"`
	YPs          []YP `toml:"yp"`
}

func defaults() Config {
	return Config{
		RTMPPort:     1935,
		PeercastPort: 7144,
		LogLevel:     "info",
	}
}

// SlogLevel converts the LogLevel string to a slog.Level.
// Accepted values: "debug", "info", "warn", "error" (case-insensitive).
// Unknown values fall back to Info.
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Load reads the TOML config file at path. Missing fields fall back to defaults.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &cfg, nil
}

// FindYP returns the YP entry matching name.
// If name is empty, the first entry is returned.
// Returns an error if the list is empty or name is not found.
func (c *Config) FindYP(name string) (*YP, error) {
	if len(c.YPs) == 0 {
		return nil, fmt.Errorf("config: no yp entries defined")
	}
	if name == "" {
		return &c.YPs[0], nil
	}
	for i := range c.YPs {
		if c.YPs[i].Name == name {
			return &c.YPs[i], nil
		}
	}
	return nil, fmt.Errorf("config: yp %q not found", name)
}
