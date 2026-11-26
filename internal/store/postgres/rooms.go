package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
)

const roomQuery = `SELECT id, owner_id, title, online, password_hash IS NOT NULL,
    (SELECT count(*) FROM room_members m WHERE m.room_id = rooms.id)
    FROM rooms`

const advanceHistoryBoundary = `history_after_id=COALESCE((SELECT max(id) FROM room_messages WHERE room_id=$1),0)`

func scanRoom(row pgx.Row) (live.Room, error) {
	var room live.Room
	err := row.Scan(&room.ID, &room.OwnerID, &room.Title, &room.Online, &room.IsLock, &room.MemberCount)
	if errors.Is(err, pgx.ErrNoRows) {
		err = live.ErrRoomNotFound
	}
	return room, err
}

func (s *Store) Info(ctx context.Context, id int64) (live.Room, error) {
	return scanRoom(s.pool.QueryRow(ctx, roomQuery+" WHERE id=$1", id))
}

func (s *Store) Rooms(ctx context.Context) ([]live.Room, error) {
	rows, err := s.pool.Query(ctx, roomQuery+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (live.Room, error) {
		return scanRoom(row)
	})
}

func (s *Store) ListRooms(ctx context.Context, filter live.RoomFilter) ([]live.Room, int64, error) {
	const statusFilter = " WHERE ($1::boolean IS NULL OR online=$1)"
	rooms := []live.Room{}
	var pages int64
	// Count and read the page from the same snapshot when rooms open or close concurrently.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM rooms"+statusFilter, filter.Online).Scan(&total); err != nil {
			return err
		}
		var offset, limit int64
		offset, limit, pages = filter.Window(total)
		if limit == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, roomQuery+statusFilter+" ORDER BY online DESC, id ASC LIMIT $2 OFFSET $3", filter.Online, limit, offset)
		if err != nil {
			return err
		}
		rooms, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (live.Room, error) {
			return scanRoom(row)
		})
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return rooms, pages, nil
}

func (s *Store) Start(ctx context.Context, owner int64, title, password string) (live.Room, error) {
	var result live.Room
	err := s.change(ctx, owner, func(r *roomChange) error {
		room, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", owner))
		newRoom := errors.Is(err, live.ErrRoomNotFound)
		if err != nil && !newRoom {
			return err
		}
		if room.Online {
			if password != "" {
				if !room.IsLock {
					return live.ErrRoomAlreadyOnline
				}
				var hash []byte
				if err := r.tx.QueryRow(ctx, "SELECT password_hash FROM rooms WHERE id=$1", owner).Scan(&hash); err != nil {
					return err
				}
				if !live.RoomPasswordMatches(hash, password) {
					return live.ErrRoomAlreadyOnline
				}
			}
			result = room
			return nil
		}
		hash, err := live.HashRoomPassword(password)
		if err != nil {
			return err
		}
		if newRoom {
			_, err = r.tx.Exec(ctx, "INSERT INTO rooms(id,owner_id,title,password_hash) VALUES($1,$1,$2,$3)", owner, title, hash)
		} else {
			for _, table := range []string{"room_members", "room_admins", "room_mutes", "room_kicks", "room_password_attempts"} {
				if _, err := r.tx.Exec(ctx, "DELETE FROM "+table+" WHERE room_id=$1", owner); err != nil {
					return err
				}
			}
			_, err = r.tx.Exec(ctx, "UPDATE rooms SET online=true,title=$2,password_hash=$3,updated_at=now(),"+advanceHistoryBoundary+" WHERE id=$1", owner, title, hash)
		}
		result = live.Room{ID: owner, OwnerID: owner, Title: title, Online: true, IsLock: len(hash) > 0}
		return err
	})
	if err != nil {
		return live.Room{}, err
	}
	return result, nil
}

func (s *Store) UpdateSettings(ctx context.Context, ownerID int64, title, password *string) (live.Room, error) {
	var result live.Room
	err := s.change(ctx, ownerID, func(r *roomChange) error {
		room, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1 AND owner_id=$1", ownerID))
		if err != nil {
			return err
		}
		if !room.Online {
			return live.ErrRoomClosed
		}
		result = room
		if title == nil && password == nil {
			return nil
		}

		var hash []byte
		if password != nil {
			hash, err = live.HashRoomPassword(*password)
			if err != nil {
				return err
			}
			result.IsLock = len(hash) > 0
			if !result.IsLock {
				if _, err := r.tx.Exec(ctx, "DELETE FROM room_password_attempts WHERE room_id=$1", ownerID); err != nil {
					return err
				}
			}
		}
		if title != nil {
			result.Title = *title
		}
		_, err = r.tx.Exec(ctx, `UPDATE rooms SET title=COALESCE($2,title),
			password_hash=CASE WHEN $3 THEN $4 ELSE password_hash END,
			updated_at=now() WHERE id=$1`, ownerID, title, password != nil, hash)
		return err
	})
	if err != nil {
		return live.Room{}, err
	}
	return result, nil
}

func (s *Store) Close(ctx context.Context, id, actor int64) error {
	return s.change(ctx, id, func(r *roomChange) error { return r.close(actor) })
}

func (r *roomChange) close(actor int64) error {
	room, err := scanRoom(r.tx.QueryRow(r.ctx, roomQuery+" WHERE id=$1", r.id))
	if err != nil {
		return err
	}
	if actor != room.OwnerID {
		return live.ErrForbidden
	}
	if !room.Online {
		return nil
	}
	if _, err = r.tx.Exec(r.ctx, "UPDATE rooms SET online=false,password_hash=NULL,updated_at=now(),"+advanceHistoryBoundary+" WHERE id=$1", r.id); err != nil {
		return err
	}
	if _, err = r.tx.Exec(r.ctx, "DELETE FROM room_members WHERE room_id=$1", r.id); err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, "DELETE FROM room_password_attempts WHERE room_id=$1", r.id)
	r.event = live.Event{Type: "close", UserID: actor}
	return err
}

func (r *roomChange) requireOnline() error {
	var online bool
	err := r.tx.QueryRow(r.ctx, "SELECT online FROM rooms WHERE id=$1", r.id).Scan(&online)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return live.ErrRoomNotFound
	case err != nil:
		return err
	case !online:
		return live.ErrRoomClosed
	}
	return nil
}

// relation changes only the allowed table names supplied by this package.
func (r *roomChange) relation(table string, uid int64, add bool) (bool, error) {
	query := "DELETE FROM " + table + " WHERE room_id=$1 AND user_id=$2"
	if add {
		query = "INSERT INTO " + table + "(room_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING"
	}
	tag, err := r.tx.Exec(r.ctx, query, r.id, uid)
	return tag.RowsAffected() > 0, err
}

func (r *roomChange) requireMember(uid int64) error {
	if err := r.requireOnline(); err != nil {
		return err
	}
	if uid == r.id {
		return nil
	}
	var member bool
	err := r.tx.QueryRow(r.ctx, "SELECT EXISTS(SELECT 1 FROM room_members WHERE room_id=$1 AND user_id=$2)", r.id, uid).Scan(&member)
	if err == nil && !member {
		return live.ErrNotMember
	}
	return err
}

func (s *Store) Join(ctx context.Context, id, uid int64, password string) error {
	var rejectedPassword bool
	err := s.change(ctx, id, func(r *roomChange) error {
		var owner int64
		var online, kicked bool
		var hash []byte
		var failedAttempts int
		err := r.tx.QueryRow(ctx, `SELECT owner_id, online, password_hash,
			EXISTS(SELECT 1 FROM room_kicks WHERE room_id=$1 AND user_id=$2),
			COALESCE((SELECT failed_attempts FROM room_password_attempts WHERE room_id=$1 AND user_id=$2), 0)
			FROM rooms WHERE id=$1`, id, uid).Scan(&owner, &online, &hash, &kicked, &failedAttempts)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return live.ErrRoomNotFound
		case err != nil:
			return err
		case !online:
			return live.ErrRoomClosed
		case uid == owner:
			return nil
		case kicked:
			return live.ErrKicked
		}
		if len(hash) > 0 {
			if failedAttempts >= live.MaxRoomPasswordAttempts {
				return live.ErrRoomPasswordLocked
			}
			if !live.RoomPasswordMatches(hash, password) {
				rejectedPassword = true
				_, err := r.tx.Exec(ctx, `INSERT INTO room_password_attempts(room_id,user_id,failed_attempts) VALUES($1,$2,1)
					ON CONFLICT (room_id,user_id) DO UPDATE SET failed_attempts=room_password_attempts.failed_attempts+1`, id, uid)
				return err
			}
			if failedAttempts > 0 {
				if _, err := r.relation("room_password_attempts", uid, false); err != nil {
					return err
				}
			}
		}
		joined, err := r.relation("room_members", uid, true)
		if joined {
			r.event = live.Event{Type: "join", UserID: uid}
		}
		return err
	})
	// Return the credential error after committing the failure count.
	if err == nil && rejectedPassword {
		return live.ErrInvalidRoomPassword
	}
	return err
}

func (s *Store) Leave(ctx context.Context, id, uid int64) error {
	return s.change(ctx, id, func(r *roomChange) error {
		if uid == id {
			return r.close(uid)
		}
		// Leaving an existing offline room is a no-op.
		if err := r.requireOnline(); errors.Is(err, live.ErrRoomClosed) {
			return nil
		} else if err != nil {
			return err
		}
		left, err := r.relation("room_members", uid, false)
		if left {
			r.event = live.Event{Type: "leave", UserID: uid}
		}
		return err
	})
}

func (s *Store) ids(ctx context.Context, table string, id int64) ([]int64, error) {
	var ids []int64
	err := s.pool.QueryRow(ctx, "SELECT ARRAY(SELECT user_id FROM "+table+" WHERE room_id=$1 ORDER BY user_id) FROM rooms WHERE id=$1", id).Scan(&ids)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, live.ErrRoomNotFound
	}
	return ids, err
}

func (s *Store) Members(ctx context.Context, id int64) ([]int64, error) {
	return s.ids(ctx, "room_members", id)
}
func (s *Store) Admins(ctx context.Context, id int64) ([]int64, error) {
	return s.ids(ctx, "room_admins", id)
}

func (s *Store) SetAdmin(ctx context.Context, id, actor, target int64, enabled bool) error {
	return s.change(ctx, id, func(r *roomChange) error {
		if err := r.requireOnline(); err != nil {
			return err
		}
		if actor != id {
			return live.ErrForbidden
		}
		_, err := r.relation("room_admins", target, enabled)
		return err
	})
}

func (r *roomChange) canModerate(actor, target int64) error {
	if err := r.requireOnline(); err != nil {
		return err
	}
	if target == r.id {
		return live.ErrForbidden
	}
	if actor == r.id {
		return nil
	}
	var admin bool
	err := r.tx.QueryRow(r.ctx, "SELECT EXISTS(SELECT 1 FROM room_admins WHERE room_id=$1 AND user_id=$2)", r.id, actor).Scan(&admin)
	if err == nil && !admin {
		return live.ErrForbidden
	}
	return err
}

func (s *Store) Mute(ctx context.Context, id, actor, target int64, duration time.Duration) error {
	return s.change(ctx, id, func(r *roomChange) error {
		if err := r.canModerate(actor, target); err != nil {
			return err
		}
		if duration == 0 {
			_, err := r.relation("room_mutes", target, false)
			return err
		}
		_, err := r.tx.Exec(ctx, `INSERT INTO room_mutes(room_id,user_id,expires_at) VALUES($1,$2,now()+make_interval(secs => $3))
            ON CONFLICT (room_id,user_id) DO UPDATE SET expires_at=EXCLUDED.expires_at`, id, target, duration.Seconds())
		return err
	})
}

func (s *Store) Kick(ctx context.Context, id, actor, target int64) error {
	return s.change(ctx, id, func(r *roomChange) error {
		if err := r.canModerate(actor, target); err != nil {
			return err
		}
		removed, err := r.relation("room_members", target, false)
		if err != nil {
			return err
		}
		if !removed {
			return live.ErrNotMember
		}
		_, err = r.relation("room_kicks", target, true)
		r.event = live.Event{Type: "kick", UserID: actor, TargetID: target}
		return err
	})
}

func (s *Store) Send(ctx context.Context, id, uid int64, text string) error {
	_, err := s.SendChat(ctx, live.ChatMessage{RoomID: id, UserID: uid, Text: text})
	return err
}

func (s *Store) SendChat(ctx context.Context, msg live.ChatMessage) (int64, error) {
	var messageID int64
	err := s.change(ctx, msg.RoomID, func(r *roomChange) error {
		if err := r.requireMember(msg.UserID); err != nil {
			return err
		}
		var muted bool
		if err := r.tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM room_mutes WHERE room_id=$1 AND user_id=$2 AND expires_at>now())", msg.RoomID, msg.UserID).Scan(&muted); err != nil {
			return err
		}
		if muted {
			return live.ErrMuted
		}
		err := r.tx.QueryRow(ctx, `INSERT INTO room_messages(room_id,user_id,text,payload)
			VALUES($1,$2,$3,$4) RETURNING id`, msg.RoomID, msg.UserID, msg.Text, msg.Payload).Scan(&messageID)
		r.event = live.Event{Type: "chat", UserID: msg.UserID, Text: msg.Text, Payload: msg.Payload}
		return err
	})
	if err != nil {
		return 0, err
	}
	return messageID, nil
}

func (s *Store) ChatHistory(ctx context.Context, roomID, userID int64, page, size uint64) ([]live.ChatMessage, error) {
	var messages []live.ChatMessage
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var online, authorized bool
		var afterID int64
		err := tx.QueryRow(ctx, `SELECT online, owner_id=$2 OR EXISTS(
			SELECT 1 FROM room_members WHERE room_id=$1 AND user_id=$2
		), history_after_id FROM rooms WHERE id=$1`, roomID, userID).Scan(&online, &authorized, &afterID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return live.ErrRoomNotFound
		case err != nil:
			return err
		case !online:
			return live.ErrRoomClosed
		case !authorized:
			return live.ErrNotMember
		}

		rows, err := tx.Query(ctx, `SELECT id,room_id,user_id,text,created_at,payload FROM room_messages
			WHERE room_id=$1 AND id>$2 ORDER BY id DESC LIMIT $3`, roomID, afterID, live.HistoryLimit)
		if err != nil {
			return err
		}
		messages, err = pgx.CollectRows(rows, pgx.RowToStructByPos[live.ChatMessage])
		return err
	})
	if err != nil {
		return nil, err
	}
	return live.HistoryPage(messages, page, size, time.Now()), nil
}

func (s *Store) Subscribe(ctx context.Context, id, uid int64) (<-chan live.Event, func(), error) {
	ch := make(chan live.Event, 32)
	err := s.change(ctx, id, func(r *roomChange) error {
		if err := r.requireMember(uid); err != nil {
			return err
		}
		r.sub, r.subUser = ch, uid
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return ch, func() {
		stream := s.stream(id)
		stream.Lock()
		defer stream.Unlock()
		listeners := stream.rooms[id]
		if _, ok := listeners[ch]; ok {
			delete(listeners, ch)
			close(ch)
		}
		if len(listeners) == 0 {
			delete(stream.rooms, id)
		}
	}, nil
}
