package httpapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/stt"
	"open_live_platform/internal/transport/httpx"
)

const sttLanguageHeader = "X-Lr-Stt-Lang"

func RegisterSTT(mux *http.ServeMux, accounts auth.AccountStore, subtitles *stt.Service, other ...*stt.Service) {
	s := server{accounts: accounts}
	mux.HandleFunc("POST /api/live/room/stt-start/{room}", s.authenticated(func(w http.ResponseWriter, r *http.Request, actor int64) {
		room, ok := sttRoomID(w, r)
		if !ok {
			return
		}
		var languages []string
		if !httpx.Decode(w, r, &languages) {
			return
		}
		if len(languages) == 0 {
			languages = []string{r.Header.Get(sttLanguageHeader)}
		}
		sttResponse(w, subtitles.Start(r.Context(), room, actor, languages, false), "4008", "rtt start error", "")
	}))
	mux.HandleFunc("GET /api/live/room/stt-query/{room}", s.authenticated(func(w http.ResponseWriter, r *http.Request, _ int64) {
		room, ok := sttRoomID(w, r)
		if !ok {
			return
		}
		status, err := subtitles.Query(r.Context(), room)
		sttResponse(w, err, "4009", "rtt query error", status)
	}))
	mux.HandleFunc("POST /api/live/room/stt-update/{room}", s.authenticated(func(w http.ResponseWriter, r *http.Request, actor int64) {
		room, ok := sttRoomID(w, r)
		if !ok {
			return
		}
		sttResponse(w, subtitles.Update(r.Context(), room, actor, []string{r.Header.Get(sttLanguageHeader)}), "4019", "rtt update error", "SUCCESS")
	}))
	mux.HandleFunc("POST /api/live/room/stt-stop/{room}", s.authenticated(func(w http.ResponseWriter, r *http.Request, actor int64) {
		room, ok := sttRoomID(w, r)
		if !ok {
			return
		}
		sttResponse(w, subtitles.Stop(r.Context(), room, actor), "4010", "rtt stop error", "")
	}))
	if secret := subtitles.CallbackSecret(); secret != "" {
		mux.HandleFunc("POST /api/live/stt/callback", sttCallback(append([]*stt.Service{subtitles}, other...), secret))
	}
}

func sttRoomID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("room"), 10, 64)
	if err != nil || id <= 0 {
		sttResponse(w, live.ErrRoomNotFound, "", "", "")
		return 0, false
	}
	return id, true
}

func sttResponse(w http.ResponseWriter, err error, code, message, data string) {
	if err == nil {
		body := map[string]string{"status": "200"}
		if data != "" {
			body["data"] = data
		}
		httpx.JSON(w, http.StatusOK, body)
		return
	}
	httpStatus := http.StatusOK
	switch {
	case errors.Is(err, live.ErrForbidden):
		httpStatus, code, message = http.StatusForbidden, "403", "forbidden"
	case errors.Is(err, live.ErrRoomNotFound), errors.Is(err, live.ErrRoomClosed):
		code, message = "4005", "data not found "
	case errors.Is(err, stt.ErrAcquire):
		code, message = "4007", err.Error()
	case errors.Is(err, stt.ErrLanguage):
		httpStatus, code, message = http.StatusBadRequest, "400", err.Error()
	}
	httpx.JSON(w, httpStatus, map[string]string{"status": code, "message": message})
}

func sttCallback(services []*stt.Service, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid Agora notification body")
			return
		}
		provided := r.Header.Get("Agora-Signature-V2")
		mac := hmac.New(sha256.New, []byte(secret))
		if provided == "" {
			provided = r.Header.Get("Agora-Signature")
			mac = hmac.New(sha1.New, []byte(secret))
		}
		mac.Write(body)
		signature, err := hex.DecodeString(provided)
		if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
			httpx.Error(w, http.StatusUnauthorized, "invalid Agora notification signature")
			return
		}
		var event struct {
			SID       string `json:"sid"`
			ProductID int    `json:"productId"`
			EventType int    `json:"eventType"`
			Payload   struct {
				AgentID string `json:"agent_id"`
				Channel string `json:"channel"`
				Status  string `json:"status"`
			}
		}
		if json.Unmarshal(body, &event) != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid Agora notification")
			return
		}
		if event.ProductID == 20 && event.EventType == 102 && event.SID != "" && event.SID == event.Payload.AgentID &&
			(event.Payload.Status == "STOPPED" || event.Payload.Status == "FAILED") {
			roomID, err := strconv.ParseInt(event.Payload.Channel, 10, 64)
			if err == nil && roomID > 0 {
				for _, subtitles := range services {
					if err := subtitles.AgentEnded(r.Context(), roomID, event.SID); err != nil {
						httpx.Error(w, http.StatusServiceUnavailable, "could not record Agora notification")
						return
					}
				}
			}
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	}
}
