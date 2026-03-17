package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var ids []int64
	for id := range cfg.AllowedUsers {
		ids = append(ids, id)
	}
	log.Printf("config: shell=%q, timeout=%s, allowed_users=%v", cfg.Shell, cfg.SessionTimeout, ids)

	sm := NewSessionManager(cfg.Shell, cfg.SessionTimeout)

	bot, err := NewBot(cfg, sm)
	if err != nil {
		log.Fatalf("bot: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Println("telsh running — press Ctrl+C to stop")
	bot.Start(ctx)

	log.Println("shutting down, closing all sessions…")
	sm.CloseAll()
	log.Println("done")
}
