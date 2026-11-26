package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

type command struct {
	Type            string `json:"type"`
	Text            string `json:"text"`
	TargetID        int64  `json:"target_id"`
	DurationSeconds int32  `json:"duration_seconds"`
	Payload         string `json:"-"`
}

// Signature signs a session token for use in the connector URL.
func Signature(token, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// Register serves room events and accepts room commands from signed sessions.
// Viewers join through the room API before connecting. The room defaults to
// the user's own room unless room_id or hash=/live/{id} is given.
func Register(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, secret, publicURL string) {
	type ticket struct {
		session string
		expires time.Time
	}
	const lifetime = time.Minute
	var tickets sync.Map
	mux.HandleFunc("GET /api/device/qrcode", func(w http.ResponseWriter, r *http.Request) {
		session, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		_, ok, err := accounts.Bearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil || secret == "" {
			httpx.Error(w, http.StatusInternalServerError, "could not issue relay ticket")
			return
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		id := "ws-" + rand.Text()
		tickets.Store(id, ticket{session, time.Now().Add(lifetime)})
		time.AfterFunc(lifetime, func() { tickets.Delete(id) })
		base := strings.TrimRight(publicURL, "/")
		if base == "" {
			scheme := "ws"
			if r.TLS != nil {
				scheme = "wss"
			}
			base = scheme + "://" + r.Host
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "200", "url": base + "/api/connector/" + id + "/" + Signature(id, secret),
		})
	})
	// Browser clients can use a separate host. The signed URL supplies credentials.
	upgrader := websocket.Upgrader{
		Subprotocols: []string{"h5relayer", "devicerelayer"},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	mux.HandleFunc("GET /api/connector/{token}/{sig}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if secret == "" || !hmac.Equal([]byte(r.PathValue("sig")), []byte(Signature(token, secret))) {
			http.Error(w, "invalid token or signature", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(token, "ws-") {
			value, found := tickets.LoadAndDelete(token)
			if !found || !time.Now().Before(value.(ticket).expires) {
				http.Error(w, "invalid or expired relay ticket", http.StatusForbidden)
				return
			}
			token = value.(ticket).session
		}
		user, ok, err := accounts.Lookup(r.Context(), token)
		if err != nil {
			http.Error(w, "authentication failed", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "invalid token or signature", http.StatusForbidden)
			return
		}

		query := r.URL.Query()
		room := query.Get("room_id")
		if channel, ok := strings.CutPrefix(query.Get("hash"), "/live/"); room == "" && ok {
			room, _, _ = strings.Cut(channel, "?")
		}
		roomID := user.ID
		if room != "" {
			var err error
			roomID, err = strconv.ParseInt(room, 10, 64)
			if err != nil {
				http.Error(w, "invalid room ID", http.StatusBadRequest)
				return
			}
		}

		events, unsubscribe, err := rooms.Subscribe(r.Context(), roomID, user.ID)
		if err != nil {
			switch {
			case errors.Is(err, live.ErrRoomNotFound):
				http.Error(w, err.Error(), http.StatusNotFound)
			case errors.Is(err, live.ErrNotMember), errors.Is(err, live.ErrRoomClosed):
				http.Error(w, err.Error(), http.StatusForbidden)
			default:
				http.Error(w, "could not subscribe to room", http.StatusInternalServerError)
			}
			return
		}
		defer unsubscribe()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			serve(r.Context(), conn, events, accounts, rooms, roomID, user, conn.Subprotocol())
		}
	})
}

func serve(ctx context.Context, conn *websocket.Conn, events <-chan live.Event, accounts auth.AccountStore, rooms live.RoomStore, roomID int64, user auth.User, protocol string) {
	defer conn.Close()
	if protocol == "h5relayer" || protocol == "devicerelayer" {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		userCommand := map[string]any{"cmd": "user", "data": map[string]any{
			"uid": strconv.FormatInt(user.ID, 10), "name": user.Account, "image": "", "debug": false,
		}}
		if protocol == "h5relayer" {
			if err := conn.WriteJSON(map[string]any{
				"content": "control", "queue": strconv.FormatInt(user.ID, 10),
				"payload": []any{userCommand},
			}); err != nil {
				return
			}
		} else if err := conn.WriteJSON(userCommand); err != nil {
			return
		}
	}
	done := make(chan struct{})
	defer close(done)
	commands := make(chan command)
	go func() {
		defer close(commands)
		for {
			if protocol == "h5relayer" {
				var raw json.RawMessage
				if err := conn.ReadJSON(&raw); err != nil {
					return
				}
				cmd, ok := parseH5Command(raw, roomID, user.ID)
				if !ok {
					continue
				}
				select {
				case commands <- cmd:
				case <-done:
					return
				}
				continue
			}
			if protocol == "devicerelayer" {
				var raw json.RawMessage
				if err := conn.ReadJSON(&raw); err != nil {
					return
				}
				cmd, ok := parseDeviceCommand(raw)
				if !ok {
					continue
				}
				select {
				case commands <- cmd:
				case <-done:
					return
				}
				continue
			}
			var cmd command
			if err := conn.ReadJSON(&cmd); err != nil {
				return
			}
			select {
			case commands <- cmd:
			case <-done:
				return
			}
		}
	}()

	for {
		var response any
		select {
		case cmd, ok := <-commands:
			if !ok {
				return
			}
			if err := cmd.apply(ctx, rooms, roomID, user.ID); err != nil {
				response = map[string]string{"type": "error", "error": err.Error()}
			} else if cmd.Type == "device" {
				response = json.RawMessage(cmd.Payload)
				if protocol == "h5relayer" {
					response = map[string]any{
						"content": "device",
						"queue":   strconv.FormatInt(user.ID, 10),
						"payload": json.RawMessage(cmd.Payload),
					}
				}
			} else {
				continue
			}
		case event, ok := <-events:
			if !ok {
				conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
			response = event
			if protocol == "h5relayer" {
				response = h5Event(ctx, accounts, roomID, event)
			}
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteJSON(response); err != nil {
			return
		}
	}
}

func (cmd command) apply(ctx context.Context, rooms live.RoomStore, roomID, userID int64) error {
	switch cmd.Type {
	case "device":
		if cmd.Payload == "" {
			return errors.New("unknown room command")
		}
		return nil
	case "chat":
		if cmd.Payload != "" {
			_, err := rooms.SendChat(ctx, live.ChatMessage{
				RoomID: roomID, UserID: userID, Text: cmd.Text, Payload: cmd.Payload,
			})
			return err
		}
		return rooms.Send(ctx, roomID, userID, cmd.Text)
	case "mute":
		return rooms.Mute(ctx, roomID, userID, cmd.TargetID, time.Duration(cmd.DurationSeconds)*time.Second)
	case "unmute":
		return rooms.Mute(ctx, roomID, userID, cmd.TargetID, 0)
	case "kick":
		return rooms.Kick(ctx, roomID, userID, cmd.TargetID)
	case "close":
		return rooms.Close(ctx, roomID, userID)
	default:
		return errors.New("unknown room command")
	}
}

func parseH5Command(raw json.RawMessage, roomID, userID int64) (command, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return command{}, false
	}
	var content string
	var payloads []json.RawMessage
	if json.Unmarshal(envelope["content"], &content) != nil ||
		(content != "live_chat" && content != "device") {
		return command{}, false
	}
	if content == "device" {
		return parseDeviceCommand(envelope["payload"])
	}
	if json.Unmarshal(envelope["payload"], &payloads) != nil || len(payloads) == 0 {
		return command{}, false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(payloads[0], &payload) != nil {
		return command{}, false
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(payload["message"], &message) != nil {
		return command{}, false
	}
	var text string
	if json.Unmarshal(message["message"], &text) != nil || text == "" {
		return command{}, false
	}
	payload["room"] = mustJSON(strconv.FormatInt(roomID, 10))
	payload["sender"] = mustJSON(strconv.FormatInt(userID, 10))
	canonical, err := json.Marshal(payload)
	if err != nil {
		return command{}, false
	}
	return command{Type: "chat", Text: text, Payload: string(canonical)}, true
}

func parseDeviceCommand(raw json.RawMessage) (command, bool) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return command{}, false
	}
	var name string
	if json.Unmarshal(value["cmd"], &name) != nil {
		return command{}, false
	}
	switch name {
	case "vibrate", "devices", "mode_vibrate", "mode_vibrate_v2", "joystick",
		"motor_strength", "connector_disconnected", "connector_connected",
		"disconnect_connector", "user", "heartbeat", "join":
		return command{Type: "device", Payload: string(raw)}, true
	default:
		return command{}, false
	}
}

func h5Event(ctx context.Context, accounts auth.AccountStore, roomID int64, event live.Event) any {
	if event.Type != "chat" {
		return event
	}
	if event.Payload != "" {
		return map[string]any{
			"content": "live_chat",
			"queue":   strconv.FormatInt(roomID, 10),
			"payload": []json.RawMessage{json.RawMessage(event.Payload)},
		}
	}
	names, _ := accounts.Names(ctx, []int64{event.UserID})
	sender := strconv.FormatInt(event.UserID, 10)
	payload := map[string]any{
		"id":        strconv.FormatInt(time.Now().UnixNano(), 10),
		"channel":   "chat",
		"room":      strconv.FormatInt(roomID, 10),
		"priority":  0,
		"sender":    sender,
		"timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"message": map[string]any{
			"msgType":  "C",
			"msgGroup": "chat",
			"userInfo": map[string]any{
				"uid": sender, "name": names[event.UserID],
			},
			"message": event.Text,
		},
	}
	return map[string]any{
		"content": "live_chat",
		"queue":   strconv.FormatInt(roomID, 10),
		"payload": []any{payload},
	}
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
