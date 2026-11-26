package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/transport/httpx"
)

func RegisterDirectMessages(mux *http.ServeMux, accounts auth.AccountStore, store dm.Store, moderator dm.Moderator, push relay.PushPrivate) {
	s := server{accounts: accounts}
	const base = "/api/dm/"
	mux.HandleFunc("POST "+base+"send_message", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		var body struct {
			ConversationID int64   `json:"conversationId,string"`
			SenderID       int64   `json:"senderId,string"`
			ReceiverID     int64   `json:"receiverId,string"`
			Type           *int8   `json:"messageType"`
			Content        *string `json:"content"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.ConversationID < 1 || body.SenderID < 1 || body.ReceiverID < 1 || body.Type == nil || body.Content == nil {
			dmErrorResponse(w, http.StatusBadRequest, "400", "invalid direct message")
			return
		}
		if body.SenderID != userID {
			dmErrorResponse(w, http.StatusUnauthorized, "401", "Unauthorized request")
			return
		}
		if err := auth.CheckCommunication(r.Context(), accounts, userID); err != nil {
			if errors.Is(err, auth.ErrCommunicationRestricted) {
				err = dm.ErrForbidden
			}
			dmReply(w, nil, err)
			return
		}
		input := dm.SendInput{ConversationID: body.ConversationID, SenderID: userID, ReceiverID: body.ReceiverID,
			Type: *body.Type, Content: *body.Content}
		if err := store.CheckDMSend(r.Context(), input); err != nil {
			dmReply(w, nil, err)
			return
		}
		if moderator == nil {
			dmModerationUnavailable(w)
			return
		}
		passed, err := moderator.ApproveDirectMessage(r.Context(), input)
		if err != nil {
			dmModerationUnavailable(w)
			return
		}
		if !passed {
			dmErrorResponse(w, http.StatusOK, "4061", "private chat audit reject")
			return
		}
		message, err := store.SendDM(r.Context(), input)
		if err == nil {
			if pushErr := push(r.Context(), body.ReceiverID, "dm", map[string]any{"message": body}); pushErr != nil {
				log.Printf("notify recipient of persisted direct message: %v", pushErr)
			}
		}
		dmReply(w, message, err)
	}))
	mux.HandleFunc("GET "+base+"page_conversation", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		id, page, ok := dmPageRequest(w, r, "userId")
		if !ok {
			return
		}
		if id != userID {
			dmErrorResponse(w, http.StatusUnauthorized, "401", "Unauthorized request")
			return
		}
		result, err := store.DMConversations(r.Context(), userID, page)
		if err != nil {
			dmReply(w, nil, err)
			return
		}
		for index := range result.Data {
			other := &result.Data[index].OtherUser
			user, exists, err := accounts.Find(r.Context(), other.ID)
			if err != nil {
				dmReply(w, nil, err)
				return
			}
			if exists {
				other.DisplayName = user.Account
				if profile := user.Profile; profile != nil {
					other.SearchName, other.Photo, other.ToyInfo, other.Anchor = profile.SearchName, profile.Photo, profile.ToyInfo, profile.IsAnchor
				}
			}
		}
		dmReply(w, result, nil)
	}))
	mux.HandleFunc("GET "+base+"page_message", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		id, page, ok := dmPageRequest(w, r, "conversationId")
		if ok {
			result, err := store.DMMessages(r.Context(), userID, id, page)
			dmReply(w, result, err)
		}
	}))
	mux.HandleFunc("GET "+base+"read_message/{conversation_id}", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		if id, ok := httpx.PathID(w, r, "conversation_id"); ok {
			dmReply(w, nil, store.ReadDM(r.Context(), userID, id))
		}
	}))
	mux.HandleFunc("GET "+base+"hide_conversation/{conversation_id}", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		if id, ok := httpx.PathID(w, r, "conversation_id"); ok {
			dmReply(w, nil, store.HideDM(r.Context(), userID, id))
		}
	}))
	mux.HandleFunc("GET "+base+"conversation/can_send/{other_user_id}", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		if otherID, ok := httpx.PathID(w, r, "other_user_id"); ok {
			_, allowed, err := store.FindDMConversation(r.Context(), userID, otherID)
			if errors.Is(err, dm.ErrNotFound) {
				err = nil
			}
			dmReply(w, allowed, err)
		}
	}))
	mux.HandleFunc("GET "+base+"conversation/get_conversation_id/{other_user_id}", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		if otherID, ok := httpx.PathID(w, r, "other_user_id"); ok {
			id, _, err := store.FindDMConversation(r.Context(), userID, otherID)
			if errors.Is(err, dm.ErrNotFound) {
				dmErrorResponse(w, http.StatusOK, "4005", fmt.Sprintf("conversation is not found, user_id: %d, other_user_id: %d not found", userID, otherID))
				return
			}
			dmReply(w, strconv.FormatInt(id, 10), err)
		}
	}))
	mux.HandleFunc("GET "+base+"conversation/get_unread_message_count", s.authenticated(func(w http.ResponseWriter, r *http.Request, userID int64) {
		count, err := store.UnreadDM(r.Context(), userID)
		dmReply(w, count, err)
	}))
}

func dmPageRequest(w http.ResponseWriter, r *http.Request, idParam string) (int64, dm.PageRequest, bool) {
	query := r.URL.Query()
	id, idErr := strconv.ParseInt(query.Get(idParam), 10, 64)
	page, pageErr := strconv.ParseInt(query.Get("page"), 10, 64)
	size, sizeErr := strconv.ParseInt(query.Get("pageSize"), 10, 64)
	if idErr != nil || id < 1 || pageErr != nil || sizeErr != nil {
		dmErrorResponse(w, http.StatusBadRequest, "400", "invalid direct message pagination")
		return 0, dm.PageRequest{}, false
	}
	return id, dm.NewPageRequest(page, size), true
}

func dmModerationUnavailable(w http.ResponseWriter) {
	dmErrorResponse(w, http.StatusServiceUnavailable, "503", "private chat audit unavailable")
}

func dmErrorResponse(w http.ResponseWriter, code int, status, message string) {
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}

func dmReply(w http.ResponseWriter, data any, err error) {
	switch {
	case errors.Is(err, dm.ErrNotFound):
		dmErrorResponse(w, http.StatusOK, "4005", "conversation not found")
	case errors.Is(err, dm.ErrForbidden):
		dmErrorResponse(w, http.StatusForbidden, "403", "Operation not allowed")
	case err != nil:
		dmErrorResponse(w, http.StatusInternalServerError, "500", "Internal error")
	default:
		result := map[string]any{"status": "200"}
		if data != nil {
			result["data"] = data
		}
		httpx.JSON(w, http.StatusOK, result)
	}
}
