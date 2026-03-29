package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/config"
	"github.com/titagaki/peercast-mm/internal/id"
	"github.com/titagaki/peercast-mm/internal/jsonrpc"
	"github.com/titagaki/peercast-mm/internal/rtmp"
	"github.com/titagaki/peercast-mm/internal/servent"
	"github.com/titagaki/peercast-mm/internal/yp"
)

func main() {
	configPath := flag.String("config", "config.toml", "Path to config file")
	ypName := flag.String("yp", "", "YP name to use (default: first entry in config)")
	chanName := flag.String("name", "", "Channel name (required)")
	chanGenre := flag.String("genre", "", "Channel genre")
	chanURL := flag.String("url", "", "Channel contact URL")
	chanDesc := flag.String("desc", "", "Channel description")
	chanBitrate := flag.Uint("bitrate", 0, "Channel bitrate (kbps)")
	flag.Parse()

	// Minimal logger before config is loaded (errors only).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	if *chanName == "" {
		slog.Error("-name is required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()})))

	sessionID := id.NewRandom()
	broadcastID := id.NewRandom()
	channelID := id.ChannelID(broadcastID, *chanName, *chanGenre, uint32(*chanBitrate))

	slog.Info("startup", "session_id", sessionID, "broadcast_id", broadcastID, "channel_id", channelID)

	ch := channel.New(channelID, broadcastID)
	ch.SetInfo(channel.ChannelInfo{
		Name:     *chanName,
		Genre:    *chanGenre,
		URL:      *chanURL,
		Desc:     *chanDesc,
		Bitrate:  uint32(*chanBitrate),
		Type:     "FLV",
		MIMEType: "video/x-flv",
		Ext:      ".flv",
	})

	// Start OutputListener.
	listener := servent.NewListener(sessionID, ch, cfg.PeercastPort, cfg.MaxRelays, cfg.MaxListeners)
	go func() {
		slog.Info("output: listening", "port", cfg.PeercastPort)
		if err := listener.ListenAndServe(); err != nil {
			slog.Error("output: listener stopped", "err", err)
		}
	}()

	// JSON-RPC API will be wired after YP setup (ypClient may be nil).
	var ypClientForAPI *yp.Client

	// Start YPClient if configured.
	ypEntry, err := cfg.FindYP(*ypName)
	if err != nil {
		slog.Info("yp: skipping", "reason", err)
	} else {
		hostPort, err := ypEntry.HostPort()
		if err != nil {
			slog.Error("yp: invalid addr", "addr", ypEntry.Addr, "err", err)
			os.Exit(1)
		}
		ypClient := yp.New(hostPort, sessionID, broadcastID, ch)
		ypClientForAPI = ypClient
		go func() {
			slog.Info("yp: connecting", "addr", ypEntry.Addr, "name", ypEntry.Name)
			ypClient.Run()
		}()
		defer ypClient.Stop()
	}

	// Wire JSON-RPC API handler into the listener.
	apiServer := jsonrpc.New(sessionID, ch, cfg, ypClientForAPI)
	listener.SetAPIHandler(apiServer.Handler())
	slog.Info("api: JSON-RPC ready", "port", cfg.PeercastPort)

	// Start RTMP server.
	rtmpServer := rtmp.NewServer(ch, cfg.RTMPPort)
	go func() {
		slog.Info("rtmp: listening", "port", cfg.RTMPPort)
		if err := rtmpServer.ListenAndServe(); err != nil {
			slog.Error("rtmp: server stopped", "err", err)
		}
	}()

	// Wait for interrupt.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down")
	rtmpServer.Close()
	listener.Close()
	ch.CloseAll()
}
