package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
)

var errRoomEventsUnavailable = errors.New("room events listener unavailable")

// roomSignal carries event metadata only. PostgreSQL limits NOTIFY payloads to
// less than 8 KB, so chat text and the original frame stay in room_messages.
type roomSignal struct {
	RoomID     int64
	Kind       string
	UserID     int64
	TargetID   int64
	MessageID  int64
	LiveID     int64 `json:",omitempty"`
	Marker     string
	ClosedAt   time.Time       `json:"ClosedAt,omitzero"`
	CreatedAt  time.Time       `json:"CreatedAt,omitzero"`
	RoomStatus live.RoomStatus `json:"RoomStatus,omitempty"`
	Likes      int64           `json:"Likes,omitempty"`
}

func (s *Store) startRoomEvents(ctx context.Context) error {
	var schemaName string
	if err := s.pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schemaName); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(schemaName))
	s.eventChannel = "open_live_rooms_" + hex.EncodeToString(hash[:16])
	conn, err := s.connectRoomEvents(ctx)
	if err != nil {
		return err
	}
	listenCtx, cancel := context.WithCancel(context.Background())
	s.stopEvents = cancel
	s.eventsDone = make(chan struct{})
	s.eventReady.Store(true)
	go s.listenRoomEvents(listenCtx, conn)
	return nil
}

func (s *Store) connectRoomEvents(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+s.eventChannel); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

func (s *Store) notifyRoomChange(r *roomChange) error {
	if r.event.Type == "" && r.sub == nil {
		if r.translationChange.Kind == "" {
			return nil
		}
		return s.notifyRoomSignal(r, r.translationChange)
	}
	if r.sub != nil {
		// Room transactions commit under the same room advisory lock. This
		// marker lets the new subscriber ignore previously committed events.
		r.subMarker = rand.Text()
	}
	signal := roomSignal{
		RoomID: r.id, Kind: r.event.Type, UserID: r.event.UserID,
		TargetID: r.event.TargetID, MessageID: r.event.MessageID, Marker: r.subMarker,
	}
	switch r.event.Type {
	case "close":
		signal.ClosedAt = r.event.CreatedAt
	case "room_status":
		signal.CreatedAt = r.event.CreatedAt
		signal.RoomStatus = r.event.RoomStatus
	case "gift":
		signal.CreatedAt = r.event.CreatedAt
	case "room_like":
		signal.CreatedAt = r.event.CreatedAt
		signal.LiveID, signal.Likes = r.event.LiveID, r.event.Likes
	case "gift_effect":
		signal.LiveID = r.event.LiveID
	}
	if err := s.notifyRoomSignal(r, signal); err != nil {
		return err
	}
	if r.translationChange.Kind != "" {
		return s.notifyRoomSignal(r, r.translationChange)
	}
	return nil
}

func (s *Store) notifyRoomSignal(r *roomChange, signal roomSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, "SELECT pg_notify($1,$2)", s.eventChannel, string(payload))
	return err
}

func (s *Store) listenRoomEvents(ctx context.Context, conn *pgx.Conn) {
	defer close(s.eventsDone)
	for {
		s.readRoomEvents(ctx, conn)
		s.eventReady.Store(false)
		s.sessionRevocations.CloseAll()
		closeCtx, cancel := context.WithTimeout(ctx, anchorHeartbeatTimeout)
		conn.Close(closeCtx)
		cancel()
		s.stopAnchorLeases()
		// LISTEN has no replay. End old sockets so reconnecting clients
		// recheck room membership and can reload the persisted chat history.
		s.closeSubscriptions()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			connectCtx, cancel := context.WithTimeout(ctx, anchorHeartbeatTimeout)
			next, err := s.connectRoomEvents(connectCtx)
			cancel()
			if err == nil {
				conn = next
				break
			}
		}
		s.eventReady.Store(true)
	}
}

func (s *Store) readRoomEvents(ctx context.Context, conn *pgx.Conn) {
	lastHeartbeat := time.Now()
	for {
		nextHeartbeat := lastHeartbeat.Add(anchorHeartbeatInterval)
		if now := time.Now(); !now.Before(nextHeartbeat) {
			if !now.Before(lastHeartbeat.Add(anchorLeaseDuration - anchorHeartbeatTimeout)) {
				// A listener stalled past its lease must drop its old sockets.
				return
			}
			heartbeatCtx, cancel := context.WithTimeout(ctx, anchorHeartbeatTimeout)
			err := s.renewAnchorLeases(heartbeatCtx, conn)
			cancel()
			if err != nil {
				return
			}
			lastHeartbeat = now
			continue
		}
		waitCtx, cancel := context.WithDeadline(ctx, nextHeartbeat)
		n, err := conn.WaitForNotification(waitCtx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			continue
		}
		if err != nil {
			return
		}
		if s.readSessionRevocation(n.Payload) {
			continue
		}
		if handled, err := s.readRelayDeviceSignal(ctx, conn, n.Payload); err != nil {
			return
		} else if handled {
			continue
		}
		var signal roomSignal
		if json.Unmarshal([]byte(n.Payload), &signal) != nil {
			continue
		}
		if err := s.deliverRoomSignal(ctx, conn, signal); err != nil {
			return
		}
	}
}

func (s *Store) deliverRoomSignal(ctx context.Context, conn *pgx.Conn, signal roomSignal) error {
	stream := s.stream(signal.RoomID)
	stream.Lock()
	defer stream.Unlock()
	listeners := stream.rooms[signal.RoomID]
	if len(listeners) == 0 {
		return nil
	}
	if signal.Marker != "" {
		for ch, sub := range listeners {
			if sub.marker == signal.Marker {
				sub.marker = ""
				listeners[ch] = sub
				break
			}
		}
		return nil
	}
	event := live.Event{Type: signal.Kind, RoomID: signal.RoomID, UserID: signal.UserID, TargetID: signal.TargetID}
	switch event.Type {
	case "chat":
		event.MessageID = signal.MessageID
		err := conn.QueryRow(ctx, "SELECT text,payload,created_at FROM room_messages WHERE id=$1 AND room_id=$2",
			event.MessageID, signal.RoomID).Scan(&event.Text, &event.Payload, &event.CreatedAt)
		if err != nil {
			return err
		}
	case "close":
		event.CreatedAt = signal.ClosedAt
		if event.CreatedAt.IsZero() {
			// Older publishers sent close events without a timestamp.
			event.CreatedAt = time.Now()
		}
	case "room_status":
		event.CreatedAt = signal.CreatedAt
		event.RoomStatus = signal.RoomStatus
	case "room_like":
		event = live.RoomLikeEvent(signal.RoomID, signal.LiveID, signal.UserID, signal.Likes, signal.CreatedAt)
	case "translate_change":
		var languages []string
		var liveID, ownerID int64
		err := conn.QueryRow(ctx, selectRoomLanguages, signal.RoomID).Scan(&languages, &liveID, &ownerID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && liveID != signal.LiveID) {
			return nil
		}
		if err != nil {
			return err
		}
		event = live.TranslationChangeEvent(ownerID, signal.CreatedAt, languages)
		event.RoomID = signal.RoomID
	case "gift":
		var result gift.Result
		err := conn.QueryRow(ctx, `SELECT g.result FROM gift_orders g JOIN rooms r ON r.id=g.room_id
			WHERE g.id=$1 AND g.room_id=$2 AND g.state=1 AND r.online AND r.live_id=g.live_id`, signal.MessageID, signal.RoomID).Scan(&result)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		event.CreatedAt = signal.CreatedAt
		event.Payload = string(result.Success)
		stream.publish(event)
		if len(result.Performance) != 0 && string(result.Performance) != "null" {
			event.Payload = string(result.Performance)
			stream.publish(event)
		}
		return nil
	case "wheel":
		var order gift.WheelOrder
		err := conn.QueryRow(ctx, `SELECT w.order_data FROM room_wheel_orders w JOIN rooms r ON r.id=w.room_id
			WHERE w.id=$1 AND w.room_id=$2 AND w.state=1 AND r.online AND r.live_id=w.live_id`, signal.MessageID, signal.RoomID).Scan(&order)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		frames, err := order.Frames()
		if err != nil {
			return err
		}
		event.Type, event.CreatedAt = "gift", order.Result.Time
		for _, frame := range frames {
			event.Payload = string(frame)
			stream.publish(event)
		}
		return nil
	case "gift_effect":
		rows, err := conn.Query(ctx, selectRoomEffects+` AND e.order_id=$3 AND EXISTS(
			SELECT 1 FROM rooms r WHERE r.id=e.room_id AND r.online AND r.live_id=e.live_id)`, signal.RoomID, signal.LiveID, signal.MessageID)
		if err != nil {
			return err
		}
		effect, err := pgx.CollectOneRow(rows, scanRoomEffect)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		frame, err := effect.HardwareFrame()
		if err != nil {
			return err
		}
		event.Type, event.Payload, event.CreatedAt = "gift", string(frame), effect.Start
	}
	stream.publish(event)
	return nil
}
