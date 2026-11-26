package app

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/liveaudit"
)

func liveAuditFromEnv() liveaudit.Config {
	enabled, _ := strconv.ParseBool(os.Getenv("SM_AUDIT_SWITCH"))
	return liveaudit.Config{Enabled: enabled, AccessKey: os.Getenv("SM_ACCESS_KEY"), AppID: os.Getenv("SM_APPID"), EventID: os.Getenv("SM_VIDEO_STREAM_EVENT_ID"),
		StartURL: os.Getenv("SM_VIDEO_STREAM_URL"), StopURL: os.Getenv("SM_CLOSE_VIDEO_STREAM_URL"), CallbackBase: os.Getenv("SM_CALLBACK_URL"),
		CallbackToken: os.Getenv("SM_LIVE_REVIEW_TOKEN"), RulesFile: os.Getenv("SM_LIVE_RULES_FILE"),
		MediaURL: os.Getenv("SM_MEDIA_URL"), ManualMediaURL: os.Getenv("SM_MANUAL_MEDIA_URL")}
}

func (a *App) monitorLiveAudit(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := a.reconcileLiveAudit(ctx); err != nil && ctx.Err() == nil {
			log.Print("could not update live media audit")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) reconcileLiveAudit(ctx context.Context) error {
	for range 32 {
		task, exists, err := a.db.ClaimLiveMediaAudit(ctx)
		if err != nil || !exists {
			return err
		}
		var requestID string
		if task.Stop {
			err = a.liveAudit.Stop(ctx, task.RequestID)
		} else {
			requestID, err = a.liveAudit.Start(ctx, task, live.DefaultAnchorNID)
		}
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		finished := a.db.FinishLiveMediaAudit(finish, task, requestID, err != nil)
		cancel()
		if finished != nil || ctx.Err() != nil {
			return finished
		}
	}
	return nil
}
