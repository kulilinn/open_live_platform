package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

// roomChat is the LiveRoomPayload exchanged by the original client.
type roomChat struct {
	ID        int64           `json:"id,string"`
	Channel   string          `json:"channel"`
	Room      int64           `json:"room,string"`
	Priority  uint32          `json:"priority"`
	Sender    int64           `json:"sender,string"`
	Timestamp int64           `json:"timestamp,string"`
	Message   json.RawMessage `json:"message"`
}

func (s server) chat(w http.ResponseWriter, r *http.Request, userID int64) {
	var raw json.RawMessage
	if !httpx.Decode(w, r, &raw) {
		return
	}
	var native struct {
		live.Event
		Channel json.RawMessage `json:"channel"`
	}
	if err := json.Unmarshal(raw, &native); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if native.RoomID != 0 || native.Channel == nil {
		if native.Text == "" {
			httpx.Error(w, http.StatusBadRequest, "chat text is required")
			return
		}
		liveResult(w, s.rooms.Send(r.Context(), native.RoomID, userID, native.Text))
		return
	}

	// A server-assigned ID in the numeric acknowledgement must fit a browser Number.
	input := roomChat{ID: rand.Int64N(1<<53-1) + 1, Timestamp: time.Now().Unix()}
	if err := json.Unmarshal(raw, &input); err != nil || input.Channel != "chat" || input.Room < 1 {
		httpx.Error(w, http.StatusBadRequest, "invalid room chat")
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(input.Message))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil {
		httpx.Error(w, http.StatusBadRequest, "expected a text chat message")
		return
	}
	text, _ := body["message"].(string)
	if text == "" {
		httpx.Error(w, http.StatusBadRequest, "chat text is required")
		return
	}
	// Hash the JSON value, so spacing and the order of object keys do not affect retries.
	input.Message, _ = json.Marshal(body)
	input.Sender = userID
	payload, err := json.Marshal(input)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid chat payload")
		return
	}
	key := struct {
		user int64
		body [32]byte
	}{userID, sha256.Sum256(input.Message)}
	_, repeated := s.recentChats.LoadOrStore(key, struct{}{})
	if !repeated {
		time.AfterFunc(2*time.Second, func() { s.recentChats.Delete(key) })
		_, err = s.rooms.SendChat(r.Context(), live.ChatMessage{
			RoomID: input.Room, UserID: userID, Text: text, Payload: string(payload),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": !repeated && err == nil, "msgId": input.ID})
}

type chatBatch struct {
	Content string             `json:"content"`
	Queue   string             `json:"queue"`
	Payload [1]json.RawMessage `json:"payload"`
}

func (s server) chatHistory(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		ID   int64  `json:"id,string"`
		Page uint64 `json:"page"`
		Size uint64 `json:"size"`
	}
	input.Size = live.HistoryLimit
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.ID < 1 {
		httpx.Error(w, http.StatusBadRequest, "room id is required")
		return
	}
	messages, err := s.rooms.ChatHistory(r.Context(), input.ID, userID, input.Page, input.Size)
	if err != nil && !errors.Is(err, live.ErrRoomNotFound) && !errors.Is(err, live.ErrRoomClosed) && !errors.Is(err, live.ErrNotMember) {
		httpx.LiveError(w, err)
		return
	}
	var ids []int64
	for _, m := range messages {
		if m.Payload == "" {
			ids = append(ids, m.UserID)
		}
	}
	names, err := s.accounts.Names(r.Context(), ids)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read sender names")
		return
	}
	queue := strconv.FormatInt(input.ID, 10)
	batches := make([]chatBatch, 0, len(messages))
	for _, m := range messages {
		payload := json.RawMessage(m.Payload)
		if m.Payload == "" {
			payload = textPayload(m, names[m.UserID])
		}
		batches = append(batches, chatBatch{"live_chat", queue, [1]json.RawMessage{payload}})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": batches})
}

func textPayload(m live.ChatMessage, name string) json.RawMessage {
	role := 2 // audience
	if m.UserID == m.RoomID {
		role = 3 // anchor
	}
	// Native Go chat stores plain text. Supply the text layout used by Rust clients.
	body, _ := json.Marshal(map[string]any{
		"msgType": "C", "msgGroup": "chat", "message": m.Text,
		"userInfo": map[string]any{
			"uid": strconv.FormatInt(m.UserID, 10), "name": name, "avatar": "", "role": role,
			"user_level": 0, "fans_level": 0, "isGuardExpired": true,
			"planName": "", "transLang": "", "transList": []string{},
		},
	})
	payload, _ := json.Marshal(roomChat{
		ID: m.ID, Channel: "chat", Room: m.RoomID, Sender: m.UserID,
		Timestamp: m.CreatedAt.Unix(), Message: body,
	})
	return payload
}
