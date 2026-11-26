package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/liveaudit"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/transport/httpx"
)

type LiveAuditStore interface {
	ApplyLiveMediaAudit(context.Context, liveaudit.Decision) (liveaudit.Task, bool, error)
}

type liveAuditRequestParams struct {
	Extra struct {
		Identity liveaudit.Identity `json:"passThrough"`
	} `json:"extra"`
}

func RegisterLiveAudit(mux *http.ServeMux, provider *liveaudit.Provider, store LiveAuditStore, push relay.PushPrivate, agoraAppID string) {
	for _, path := range []string{liveaudit.StreamPath, liveaudit.ManualPath} {
		mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
			if provider == nil || store == nil {
				httpx.Error(w, http.StatusServiceUnavailable, "live media audit is unavailable")
				return
			}
			if !httpx.RequireToken(w, r, provider.CallbackToken) {
				return
			}
			payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
			if err != nil {
				httpx.Error(w, http.StatusBadRequest, "invalid live media audit callback")
				return
			}
			decision, valid := parseLiveAuditDecision(provider, path, payload)
			if !valid {
				httpx.Error(w, http.StatusBadRequest, "invalid live media audit callback")
				return
			}
			task, applied, err := store.ApplyLiveMediaAudit(r.Context(), decision)
			if errors.Is(err, liveaudit.ErrUnknown) {
				httpx.Error(w, http.StatusNotFound, "live media audit request not found")
				return
			}
			if err != nil {
				httpx.Error(w, http.StatusServiceUnavailable, "live media audit decision unavailable")
				return
			}
			if applied && decision.Operation == 1 && push != nil {
				now := time.Now().Unix()
				_ = push(r.Context(), task.OwnerID, "streamer_alert", map[string]any{"app_id": agoraAppID,
					"room_id": decision.Identity.Channel, "uid": decision.Identity.AnchorID, "reason": decision.Reasons,
					"limit_time": now, "limit_total": 0, "ts": now})
			}
			smAuditAcknowledged(w)
		})
	}
}

func parseLiveAuditDecision(provider *liveaudit.Provider, path string, payload []byte) (liveaudit.Decision, bool) {
	decision := liveaudit.Decision{Kind: path, Duration: 3600, Payload: payload}
	if path == liveaudit.ManualPath {
		var input struct {
			ID     string `json:"requestId"`
			Result *struct {
				Operation   int    `json:"operation"`
				Duration    *int64 `json:"duration"`
				Tips        string `json:"tips"`
				Description string `json:"description"`
			} `json:"result"`
			Data struct {
				StreamID string             `json:"streamId"`
				Identity liveaudit.Identity `json:"passThrough"`
			} `json:"data"`
		}
		if json.Unmarshal(payload, &input) != nil || input.ID == "" || input.Result == nil || input.Result.Operation < 1 || input.Result.Operation > 6 {
			return decision, false
		}
		decision.Identity = input.Data.Identity
		decision.ProviderID = input.Data.StreamID
		if decision.ProviderID == "" {
			decision.ProviderID = input.ID
		}
		decision.EventID = "manual:" + input.ID
		if input.Result.Duration != nil {
			decision.Duration = *input.Result.Duration
		}
		if input.Result.Operation <= 4 {
			decision.Operation = input.Result.Operation
		}
		reason := input.Result.Tips
		if reason == "" {
			reason = input.Result.Description
		}
		decision.Reasons = []string{reason}
	} else {
		var input struct {
			ID          string `json:"requestId"`
			Code        int    `json:"code"`
			State       *int   `json:"statCode"`
			ContentType int    `json:"contentType"`
			PullSuccess *bool  `json:"pullStreamSuccess"`
			Aux         struct {
				Identity liveaudit.Identity `json:"passThrough"`
			} `json:"auxInfo"`
			Image  *liveaudit.Evidence    `json:"frameDetail"`
			Audio  *liveaudit.Evidence    `json:"audioDetail"`
			Params liveAuditRequestParams `json:"requestParams"`
			End    struct {
				Params liveAuditRequestParams `json:"requestParams"`
			} `json:"detail"`
		}
		if json.Unmarshal(payload, &input) != nil || input.ID == "" || input.Code != 1100 || input.State == nil {
			return decision, false
		}
		decision.ProviderID = input.ID
		data := sha256.Sum256(payload)
		decision.EventID = "auto:" + hex.EncodeToString(data[:])
		switch *input.State {
		case 0:
			decision.Identity = input.Aux.Identity
			decision.Reasons = provider.BlockReasons(input.Image, input.Audio)
			if len(decision.Reasons) > 0 {
				decision.Operation = 3
			}
		case 1:
			if input.PullSuccess == nil || input.ContentType != 1 && input.ContentType != 2 {
				return decision, false
			}
			decision.Identity = input.Params.Extra.Identity
			if input.ContentType == 1 {
				decision.Identity = input.End.Params.Extra.Identity
				decision.Retry = !*input.PullSuccess
			}
		default:
			return decision, false
		}
	}
	room, roomErr := strconv.ParseInt(decision.Identity.Channel, 10, 64)
	anchor, anchorErr := strconv.ParseInt(decision.Identity.AnchorID, 10, 64)
	valid := roomErr == nil && anchorErr == nil && room > 0 && anchor > 0 && len(decision.ProviderID) <= 255 &&
		decision.Duration >= -1 && decision.Duration <= math.MaxInt32 && (decision.Operation < 2 || decision.Duration != 0)
	return decision, valid
}
