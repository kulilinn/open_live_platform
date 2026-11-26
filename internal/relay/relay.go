package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
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
	DeviceTarget    int64  `json:"-"`
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
func Register(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, secret, publicURL string, options ...RustOptions) PushPrivate {
	type ticket struct {
		session string
		expires time.Time
	}
	const lifetime = time.Minute
	var tickets sync.Map
	var rust *rustTickets
	var devices deviceHub
	if len(options) != 0 {
		devices.configure(options[0])
	}
	if len(options) != 0 && options[0].AppKey != "" && options[0].AppSecret != "" {
		rust = newRustTickets(options[0])
		rust.registerControls(mux, accounts, &devices, publicURL)
		mux.HandleFunc("POST /api/device/logs", rust.deviceLogs(accounts, options[0].DeviceLogs))
		mux.HandleFunc("GET /relay/verify/{token}/{sig}", func(w http.ResponseWriter, r *http.Request) {
			rust.verifyHTTP(accounts, w, r)
		})
		mux.HandleFunc("GET /api/device/toyinfo", func(w http.ResponseWriter, r *http.Request) {
			user, ok, err := accounts.Bearer(r.Context(), r.Header.Get("Authorization"))
			if err != nil {
				httpx.Error(w, http.StatusServiceUnavailable, "authentication unavailable")
				return
			}
			if !ok || user.Moderation.Banned() {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			devices.sync(r.Context(), user.ID)
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
		})
	}
	mux.HandleFunc("GET /api/device/qrcode", func(w http.ResponseWriter, r *http.Request) {
		session, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		user, ok, err := accounts.Bearer(r.Context(), r.Header.Get("Authorization"))
		if err != nil || (secret == "" && rust == nil) {
			httpx.Error(w, http.StatusInternalServerError, "could not issue relay ticket")
			return
		}
		if !ok || user.Moderation.Banned() {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if rust != nil {
			login, ok := auth.RelaySessionForToken(accounts, session)
			if !ok {
				httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
				return
			}
			ticket, err := rust.issue(r.Context(), login, user.ID, time.Now())
			if err != nil {
				httpx.Error(w, http.StatusServiceUnavailable, "relay tickets unavailable")
				return
			}
			httpx.JSON(w, http.StatusOK, map[string]string{
				"status": "200", "url": connectorURL(r, publicURL, ticket.Token, ticket.signature),
			})
			return
		}
		id := "ws-" + rand.Text()
		tickets.Store(id, ticket{session, time.Now().Add(lifetime)})
		time.AfterFunc(lifetime, func() { tickets.Delete(id) })
		httpx.JSON(w, http.StatusOK, map[string]string{
			"status": "200", "url": connectorURL(r, publicURL, id, Signature(id, secret)),
		})
	})
	// Browser clients can use a separate host. The signed URL supplies credentials.
	upgrader := websocket.Upgrader{
		Subprotocols: []string{"h5relayer", "devicerelayer"},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	mux.HandleFunc("GET /api/connector/{token}/{sig}", func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		signature := r.PathValue("sig")
		var compatible *rustTicket
		if rust != nil {
			var err error
			compatible, err = rust.verify(r.Context(), token, signature, time.Now())
			if err != nil {
				http.Error(w, "relay tickets unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if compatible == nil && (secret == "" || !hmac.Equal([]byte(signature), []byte(Signature(token, secret)))) {
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
		var sessionCtx context.Context
		var unwatch func()
		var user auth.User
		var ok bool
		var err error
		if compatible != nil {
			sessionCtx, unwatch = compatible.Login.Watch(r.Context(), accounts)
			user, ok, err = rust.loginUser(r.Context(), accounts, compatible)
		} else {
			sessionCtx, unwatch = auth.WatchSession(r.Context(), accounts, token)
			user, ok, err = accounts.Lookup(r.Context(), token)
		}
		defer unwatch()
		if err != nil {
			http.Error(w, "authentication failed", http.StatusInternalServerError)
			return
		}
		if !ok || user.Moderation.Banned() {
			http.Error(w, "invalid token or signature", http.StatusForbidden)
			return
		}

		query := r.URL.Query()
		room := query.Get("room_id")
		if channel, ok := strings.CutPrefix(query.Get("hash"), "/live/"); room == "" && ok {
			room, _, _ = strings.Cut(channel, "?")
		}
		roomID := user.ID
		if compatible != nil && room == "" {
			roomID = 0
		}
		if room != "" {
			roomID, err = strconv.ParseInt(room, 10, 64)
			if err != nil || roomID <= 0 {
				http.Error(w, "invalid room ID", http.StatusBadRequest)
				return
			}
		}

		subscribe := rooms.SubscribeLive
		if slices.Contains(websocket.Subprotocols(r), "h5relayer") {
			subscribe = rooms.SubscribeWithTranslations
		}
		var sub live.Subscription
		if roomID != 0 {
			sub, err = subscribe(r.Context(), roomID, user.ID)
		}
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
		if sub.Unsubscribe != nil {
			defer sub.Unsubscribe()
		}

		switch cause := context.Cause(sessionCtx); {
		case errors.Is(cause, auth.ErrSessionRevoked):
			http.Error(w, "invalid token or signature", http.StatusForbidden)
			return
		case cause != nil:
			http.Error(w, "session unavailable", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			protocol := conn.Subprotocol()
			var compatibility *rustSocket
			if compatible != nil {
				if protocol == "" {
					protocol = "devicerelayer"
				}
				compatibility = &rustSocket{devices: &devices, tickets: rust, ticket: compatible}
			}
			stop := context.AfterFunc(sessionCtx, func() {
				cause := context.Cause(sessionCtx)
				switch {
				case errors.Is(cause, auth.ErrSessionRevoked):
					code, reason := websocket.ClosePolicyViolation, cause.Error()
					if protocol == "h5relayer" || protocol == "devicerelayer" {
						code, reason = websocket.CloseNormalClosure, "SERVICE_LOGOUT"
					}
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
				case errors.Is(cause, auth.ErrSessionWatchUnavailable):
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, cause.Error()), time.Now().Add(time.Second))
				}
				_ = conn.Close()
			})
			defer stop()
			var private <-chan json.RawMessage
			if protocol == "h5relayer" {
				var stopPrivate func()
				private, stopPrivate = devices.private.subscribe(user.ID)
				defer stopPrivate()
			}
			serve(sessionCtx, conn, sub, accounts, rooms, roomID, user, protocol, compatibility, private)
		}
	})
	return devices.pushPrivate
}

type rustSocket struct {
	devices *deviceHub
	tickets *rustTickets
	ticket  *rustTicket
}

func serve(ctx context.Context, conn *websocket.Conn, sub live.Subscription, accounts auth.AccountStore, rooms live.RoomStore, roomID int64, user auth.User, protocol string, compatibility *rustSocket, private <-chan json.RawMessage) {
	defer conn.Close()
	commandCtx := live.WithRoomCommandScope(ctx, roomID, sub.LiveID)
	var anchorPing <-chan time.Time
	isAnchor := protocol == "h5relayer" && roomID == user.ID && sub.LiveID != 0
	if isAnchor {
		connectionID := rand.Text()
		if err := rooms.AnchorConnected(ctx, roomID, sub.LiveID, connectionID); err != nil {
			conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "could not register anchor connection"), time.Now().Add(time.Second))
			return
		}
		defer func() {
			disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			if err := rooms.AnchorDisconnected(disconnectCtx, roomID, sub.LiveID, connectionID); err != nil {
				log.Printf("disconnect anchor in room %d: %v", roomID, err)
			}
		}()
	}
	if isAnchor || compatibility != nil {
		const pongTimeout = 75 * time.Second
		if err := conn.SetReadDeadline(time.Now().Add(pongTimeout)); err != nil {
			return
		}
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongTimeout))
		})
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		anchorPing = ticker.C
	}
	if protocol == "h5relayer" || protocol == "devicerelayer" {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		name := user.Account
		debug := false
		if compatibility != nil {
			name = ""
			debug = compatibility.ticket.Debug
		}
		userCommand := map[string]any{"cmd": "user", "data": map[string]any{
			"uid": strconv.FormatInt(user.ID, 10), "name": name, "image": "", "debug": debug,
		}}
		if protocol == "h5relayer" {
			var payload any = []any{userCommand}
			if compatibility != nil {
				payload = userCommand
			}
			if err := conn.WriteJSON(map[string]any{
				"content": "control", "queue": strconv.FormatInt(user.ID, 10),
				"payload": payload,
			}); err != nil {
				return
			}
		} else if err := conn.WriteJSON(userCommand); err != nil {
			return
		}
	}
	var device *devicePeer
	var deviceMessages <-chan any
	var refreshTicks <-chan time.Time
	if compatibility != nil {
		device = compatibility.devices.join(ctx, user.ID, protocol == "devicerelayer")
		defer device.close()
		deviceMessages = device.messages
		refreshInterval := min(time.Minute, max(time.Millisecond, compatibility.tickets.ttl/2))
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		refreshTicks = ticker.C
	}
	parseH5 := parseH5Command
	if compatibility != nil {
		parseH5 = parseRustH5Command
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
				cmd, ok := parseH5(raw, roomID, user.ID)
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
		case <-refreshTicks:
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			compatibility.tickets.refresh(ctx, compatibility.ticket, time.Now())
			cancel()
			continue
		case response = <-deviceMessages:
		case response = <-private:
		case <-anchorPing:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
			if device != nil && device.device {
				device.hub.publishPresence(ctx, device.uid)
			}
			continue
		case cmd, ok := <-commands:
			if !ok {
				return
			}
			if ctx.Err() != nil {
				// The cancellation callback sends the close frame and ends the
				// read loop. Drop commands received during that interval.
				continue
			}
			if device != nil && cmd.Type == "device" {
				device.publish(ctx, cmd.DeviceTarget, cmd.Payload)
				continue
			}
			var commandErr error
			if cmd.Type == "chat" {
				commandErr = auth.CheckCommunication(commandCtx, accounts, user.ID)
			}
			if commandErr == nil {
				commandErr = cmd.apply(commandCtx, rooms, roomID, user.ID)
			}
			if commandErr != nil {
				response = map[string]string{"type": "error", "error": commandErr.Error()}
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
		case event, ok := <-sub.Events:
			if !ok {
				conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
			if (event.Type == "room_status" || event.Type == "translate_change" || event.Type == "gift") && protocol != "h5relayer" {
				continue
			}
			response = event
			if protocol == "h5relayer" {
				response = h5Event(ctx, accounts, roomID, event)
			}
		}
		if ctx.Err() != nil {
			continue
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
	room := strconv.FormatInt(roomID, 10)
	if event.Type == "gift" {
		return h5RoomFrame(room, json.RawMessage(event.Payload))
	}
	if event.Type == "close" || event.Type == "room_status" || event.Type == "translate_change" {
		channel, messageType := "room_status", "O"
		var message any = strconv.Itoa(int(event.RoomStatus))
		if event.Type == "translate_change" {
			channel, messageType, message = "translate_change", "T", json.RawMessage(event.Payload)
		}
		return h5RoomFrame(room, map[string]any{
			"id":        strconv.FormatInt(event.CreatedAt.UnixNano(), 10),
			"channel":   channel,
			"room":      room,
			"priority":  0,
			"sender":    strconv.FormatInt(event.UserID, 10),
			"timestamp": strconv.FormatInt(event.CreatedAt.Unix(), 10),
			"message": map[string]any{
				"msgGroup": "chat",
				"msgType":  messageType,
				"message":  message,
				"userInfo": map[string]any{
					"uid": event.UserID, "role": 5, "avatar": nil, "searchName": nil,
					"name": nil, "country": nil, "userLevel": nil, "fanLevel": nil,
				},
			},
		})
	}
	if event.Type != "chat" {
		return event
	}
	if event.Payload != "" {
		return h5RoomFrame(room, json.RawMessage(event.Payload))
	}
	names, _ := accounts.Names(ctx, []int64{event.UserID})
	sender := strconv.FormatInt(event.UserID, 10)
	payload := map[string]any{
		"id":        strconv.FormatInt(event.MessageID, 10),
		"channel":   "chat",
		"room":      room,
		"priority":  0,
		"sender":    sender,
		"timestamp": strconv.FormatInt(event.CreatedAt.Unix(), 10),
		"message": map[string]any{
			"msgType":  "C",
			"msgGroup": "chat",
			"userInfo": map[string]any{
				"uid": sender, "name": names[event.UserID],
			},
			"message": event.Text,
		},
	}
	return h5RoomFrame(room, payload)
}

func h5RoomFrame(room string, payload any) map[string]any {
	return map[string]any{
		"content": "live_chat",
		"queue":   room,
		"payload": []any{payload},
	}
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
