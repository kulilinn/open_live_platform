package app

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/notifyemail"
)

func roomModerationMailFromEnv() notifyemail.Config {
	host, path := os.Getenv("LOVEROSE_PUSH_HOST"), os.Getenv("LOVEROSE_PUSH_PATH")
	if host == "" || path == "" {
		return notifyemail.Config{}
	}
	second := func(variable, fallback string) string {
		fields := strings.Split(os.Getenv(variable), ";")
		if len(fields) > 1 && strings.TrimSpace(fields[1]) != "" {
			return strings.TrimSpace(fields[1])
		}
		return fallback
	}
	return notifyemail.Config{URL: host + path, From: notifyemail.Address{
		Address: second("LOVEROSE_PUSH_EMAIL", "noreply@rose.love"),
		Name:    second("LOVEROSE_PUSH_EMAIL_NAME", "Rose.Love"),
	}}
}

func (a *App) monitorRoomModerationMail(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := admin.DeliverRoomModerationMail(ctx, a.db, a.roomModerationMailer); err != nil && ctx.Err() == nil {
			log.Print("room moderation email could not be delivered")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
