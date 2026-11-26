package httpapi

import (
	"log"
	"net/http"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/botchat"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/transport/httpx"
)

func RegisterBotChat(mux *http.ServeMux, accounts auth.AccountStore, store botchat.Store, push relay.PushPrivate) {
	s := server{accounts: accounts}
	mux.HandleFunc("POST /api/chatbot/{botid}", s.authenticated(func(w http.ResponseWriter, r *http.Request, user int64) {
		bot, ok := httpx.PathID(w, r, "botid")
		if !ok {
			return
		}
		var body struct {
			Request  *string
			Language string
		}
		body.Language = "ja"
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.Request == nil {
			httpx.Error(w, 400, "request required")
			return
		}
		if err := auth.CheckCommunication(r.Context(), accounts, user); err != nil {
			httpx.Error(w, 403, "messaging unavailable")
			return
		}
		m := botchat.NewMessage(user, bot, *body.Request, body.Language)
		if err := store.SaveBotChat(r.Context(), m); err != nil {
			httpx.Error(w, 500, "could not save message")
			return
		}
		frame := relay.QueuedPayload{Queue: user, Payload: m}
		deliver := func(to int64) {
			if err := push(r.Context(), to, "botchat", frame); err != nil {
				log.Printf("notify recipient of persisted bot chat: %v", err)
			}
		}
		deliver(user)
		if bot != user {
			deliver(bot)
		}
		httpx.JSON(w, 200, map[string]string{"status": "200", "data": m.ID})
	}))
	mux.HandleFunc("GET /api/chatbot/{botid}/{token}", s.authenticated(func(w http.ResponseWriter, r *http.Request, user int64) {
		bot, ok := httpx.PathID(w, r, "botid")
		if !ok {
			return
		}
		after, err := botchat.After(user, bot, r.PathValue("token"))
		if err != nil {
			httpx.Error(w, 400, err.Error())
			return
		}
		items, err := store.BotChatHistory(r.Context(), user, bot, after)
		if err != nil {
			httpx.Error(w, 500, "could not read messages")
			return
		}
		httpx.JSON(w, 200, botchat.Paginate(items))
	}))
}
