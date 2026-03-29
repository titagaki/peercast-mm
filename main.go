package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/titagaki/peercast-mm/internal/channel"
	"github.com/titagaki/peercast-mm/internal/id"
	"github.com/titagaki/peercast-mm/internal/output"
	"github.com/titagaki/peercast-mm/internal/rtmp"
	"github.com/titagaki/peercast-mm/internal/yp"
)

func main() {
	ypAddr := flag.String("yp", "", "YP address (host:port), e.g. yp.example.com:7144")
	chanName := flag.String("name", "test", "Channel name")
	chanGenre := flag.String("genre", "", "Channel genre")
	chanURL := flag.String("url", "", "Channel contact URL")
	chanDesc := flag.String("desc", "", "Channel description")
	flag.Parse()

	sessionID := id.NewRandom()
	broadcastID := id.NewRandom()
	channelID := id.ChannelID(broadcastID, *chanName, *chanGenre, "")

	log.Printf("SessionID:   %s", sessionID)
	log.Printf("BroadcastID: %s", broadcastID)
	log.Printf("ChannelID:   %s", channelID)

	ch := channel.New(channelID, broadcastID)
	ch.SetInfo(channel.ChannelInfo{
		Name:     *chanName,
		Genre:    *chanGenre,
		URL:      *chanURL,
		Desc:     *chanDesc,
		Type:     "FLV",
		MIMEType: "video/x-flv",
		Ext:      ".flv",
	})

	// Start OutputListener.
	listener := output.NewListener(sessionID, ch)
	go func() {
		log.Printf("output: listening on :7144")
		if err := listener.ListenAndServe(); err != nil {
			log.Printf("output: listener stopped: %v", err)
		}
	}()

	// Start YPClient if configured.
	if *ypAddr != "" {
		ypClient := yp.New(*ypAddr, sessionID, broadcastID, ch)
		go func() {
			log.Printf("yp: connecting to %s", *ypAddr)
			ypClient.Run()
		}()
		defer ypClient.Stop()
	}

	// Start RTMP server.
	rtmpServer := rtmp.NewServer(ch)
	go func() {
		log.Printf("rtmp: listening on :1935")
		if err := rtmpServer.ListenAndServe(); err != nil {
			log.Printf("rtmp: server stopped: %v", err)
		}
	}()

	// Wait for interrupt.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Println("shutting down...")
	rtmpServer.Close()
	listener.Close()
	ch.CloseAll()
}
