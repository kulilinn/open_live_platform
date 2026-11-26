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

	"open_live_platform/internal/live"
)

var errRoomEventsUnavailable = errors.New("room events listener unavailable")

// roomSignal carries identifiers only. PostgreSQL limits NOTIFY payloads to
// less than 8 KB, so chat text and the original frame stay in room_messages.
type roomSignal struct {
	RoomID    int64
	Kind      string
	UserID    int64
	TargetID  int64
	MessageID int64
	Marker    string
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
		return nil
	}
	if r.sub != nil {
		// Room transactions commit under the same room advisory lock. This
		// marker lets the new subscriber ignore previously committed events.
		r.subMarker = rand.Text()
	}
	signal, err := json.Marshal(roomSignal{
		RoomID: r.id, Kind: r.event.Type, UserID: r.event.UserID,
		TargetID: r.event.TargetID, MessageID: r.event.MessageID, Marker: r.subMarker,
	})
	if err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, "SELECT pg_notify($1,$2)", s.eventChannel, string(signal))
	return err
}

func (s *Store) listenRoomEvents(ctx context.Context, conn *pgx.Conn) {
	defer close(s.eventsDone)
	for {
		s.readRoomEvents(ctx, conn)
		s.eventReady.Store(false)
		conn.Close(ctx)
		// LISTEN has no replay. End old sockets so reconnecting clients
		// recheck room membership and can reload the persisted chat history.
		s.closeSubscriptions()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if next, err := s.connectRoomEvents(ctx); err == nil {
				conn = next
				break
			}
		}
		s.eventReady.Store(true)
	}
}

func (s *Store) readRoomEvents(ctx context.Context, conn *pgx.Conn) {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
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
				listeners[ch] = subscriber{userID: sub.userID}
				break
			}
		}
		return nil
	}
	event := live.Event{Type: signal.Kind, RoomID: signal.RoomID, UserID: signal.UserID, TargetID: signal.TargetID}
	if event.Type == "chat" {
		event.MessageID = signal.MessageID
		err := conn.QueryRow(ctx, "SELECT text,payload,created_at FROM room_messages WHERE id=$1 AND room_id=$2",
			event.MessageID, signal.RoomID).Scan(&event.Text, &event.Payload, &event.CreatedAt)
		if err != nil {
			return err
		}
	}
	stream.publish(event)
	return nil
}
