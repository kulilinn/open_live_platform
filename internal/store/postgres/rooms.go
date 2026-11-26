package postgres

import (
	"cmp"
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/live"
)

const roomFields = `id, owner_id, title, online, password_hash IS NOT NULL,
    (SELECT count(*) FROM room_members m WHERE m.room_id = rooms.id),
    live_id, live_started_at, live_ended_at, trans_lang, anchor_away, anchor_away_until,
    banner, tag, description, offline_info, plan::text, gifts::text, trans_enabled,
    moderation_until, moderation_total, moderation_reason, live_show, teacher_label`

const roomQuery = "SELECT " + roomFields + " FROM rooms"

const advanceHistoryBoundary = `history_after_id=COALESCE((SELECT max(id) FROM room_messages WHERE room_id=$1),0)`

func scanRoom(row pgx.Row) (live.Room, error) {
	var room live.Room
	var startedAt, endedAt, awayUntil *time.Time
	err := row.Scan(&room.ID, &room.OwnerID, &room.Title, &room.Online, &room.IsLock, &room.MemberCount,
		&room.Session.LiveID, &startedAt, &endedAt, &room.TranslationLanguage, &room.AnchorAway, &awayUntil,
		&room.Banner, &room.Tag, &room.Description, &room.OfflineInfo, &room.Plan, &room.Gifts, &room.TranslationEnabled,
		&room.ModerationUntil, &room.ModerationTotal, &room.ModerationReason, &room.LiveShow, &room.TeacherLabel)
	if errors.Is(err, pgx.ErrNoRows) {
		err = live.ErrRoomNotFound
	}
	if startedAt != nil {
		room.Session.StartedAt = startedAt.UTC()
		room.Session.AnchorNID = live.DefaultAnchorNID
	}
	if endedAt != nil {
		room.Session.EndedAt = endedAt.UTC()
	}
	if awayUntil != nil {
		room.AnchorAwayUntil = awayUntil.UTC()
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
	const roomFilter = " WHERE ($1::boolean IS NULL OR online=$1) AND ($2::text IS NULL OR tag=$2)"
	rooms := []live.Room{}
	var pages int64
	// Count and read the page from the same snapshot when rooms open or close concurrently.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM rooms"+roomFilter, filter.Online, filter.Tag).Scan(&total); err != nil {
			return err
		}
		var offset, limit int64
		offset, limit, pages = filter.Window(total)
		if limit == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, roomQuery+roomFilter+" ORDER BY online DESC, id ASC LIMIT $3 OFFSET $4", filter.Online, filter.Tag, limit, offset)
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
	return s.StartWithLanguage(ctx, owner, title, password, nil)
}

func (s *Store) StartWithLanguage(ctx context.Context, owner int64, title, password string, language *string) (live.Room, error) {
	return s.StartWithSettings(ctx, owner, live.RoomSettings{Title: &title, Password: &password, TranslationLanguage: language}, false)
}

func (s *Store) StartWithSettings(ctx context.Context, owner int64, settings live.RoomSettings, requireOffline bool) (live.Room, error) {
	var password string
	if settings.Password != nil {
		password = *settings.Password
	}
	var result live.Room
	err := s.change(ctx, owner, func(r *roomChange) error {
		var cancelled bool
		if err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM anchor_review_cancellations c
			WHERE c.user_id=$1 AND NOT EXISTS(SELECT 1 FROM anchor_profiles a WHERE a.user_id=c.user_id AND a.state=1))`, owner).Scan(&cancelled); err != nil {
			return err
		}
		if cancelled {
			return live.ErrForbidden
		}
		room, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1", owner))
		newRoom := errors.Is(err, live.ErrRoomNotFound)
		if err != nil && !newRoom {
			return err
		}
		if room.Limited(time.Now()) {
			return live.ErrRoomLimited
		}
		if room.Online {
			if requireOffline {
				return live.ErrRoomAlreadyOnline
			}
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
			language := ""
			if requireOffline {
				language = "en"
			}
			room, err = r.createOfflineRoom(language)
			if err != nil {
				return err
			}
		} else {
			for _, table := range []string{"room_members", "room_admins", "room_mutes", "room_kicks", "room_password_attempts", "room_viewer_nids", "room_anchor_connections"} {
				if _, err := r.tx.Exec(ctx, "DELETE FROM "+table+" WHERE room_id=$1", owner); err != nil {
					return err
				}
			}
		}
		room.ApplySettings(settings)
		result, err = r.writeSettings(room, hash, true, true)
		if err != nil {
			return err
		}
		_, err = r.tx.Exec(ctx, `INSERT INTO room_broadcast_metrics(live_id,room_id,started_at)
			VALUES($1,$2,$3)`, result.Session.LiveID, r.id, result.Session.StartedAt)
		return err
	})
	if err != nil {
		return live.Room{}, err
	}
	return result, nil
}

func (s *Store) UpdateSettings(ctx context.Context, ownerID int64, settings live.RoomSettings) (live.Room, error) {
	return s.updateSettings(ctx, ownerID, settings, false)
}

func (s *Store) ConfigureRoom(ctx context.Context, ownerID int64, settings live.RoomSettings) (live.Room, error) {
	return s.updateSettings(ctx, ownerID, settings, true)
}

func (s *Store) updateSettings(ctx context.Context, ownerID int64, settings live.RoomSettings, allowOffline bool) (live.Room, error) {
	var result live.Room
	err := s.change(ctx, ownerID, func(r *roomChange) error {
		room, err := scanRoom(r.tx.QueryRow(ctx, roomQuery+" WHERE id=$1 AND owner_id=$1", ownerID))
		if allowOffline && errors.Is(err, live.ErrRoomNotFound) {
			room, err = r.createOfflineRoom("en")
		}
		if err != nil {
			return err
		}
		if !allowOffline && !room.Online {
			return live.ErrRoomClosed
		}
		result = room
		password := settings.Password
		if settings == (live.RoomSettings{}) {
			return nil
		}

		var hash []byte
		if password != nil {
			hash, err = live.HashRoomPassword(*password)
			if err != nil {
				return err
			}
			if len(hash) == 0 {
				if _, err := r.tx.Exec(ctx, "DELETE FROM room_password_attempts WHERE room_id=$1", ownerID); err != nil {
					return err
				}
			}
		}
		room.ApplySettings(settings)
		if room.Online && live.TranslationPreferenceChanged(result.TranslationEnabled, room.TranslationEnabled,
			result.TranslationLanguage, room.TranslationLanguage) {
			before, err := r.translationLanguages()
			if err != nil {
				return err
			}
			language := live.ActiveTranslationLanguage(room.TranslationEnabled, room.TranslationLanguage)
			if _, err = r.tx.Exec(ctx, "UPDATE rooms SET anchor_trans_lang=$2 WHERE id=$1", ownerID, language); err != nil {
				return err
			}
			if _, err = r.recordTranslationChange(before); err != nil {
				return err
			}
		}
		result, err = r.writeSettings(room, hash, password != nil, false)
		return err
	})
	if err != nil {
		return live.Room{}, err
	}
	return result, nil
}

func (r *roomChange) createOfflineRoom(language string) (live.Room, error) {
	return scanRoom(r.tx.QueryRow(r.ctx, `INSERT INTO rooms(id,owner_id,title,online,trans_lang)
		VALUES($1,$1,'',false,$2) RETURNING `+roomFields, r.id, language))
}

func (r *roomChange) writeSettings(room live.Room, hash []byte, replacePassword, start bool) (live.Room, error) {
	query := `UPDATE rooms SET title=$2,password_hash=CASE WHEN $3 THEN $4 ELSE password_hash END,
		trans_lang=$5,banner=$6,tag=$7,description=$8,offline_info=$9,plan=$10,gifts=$11,trans_enabled=$12,
		updated_at=statement_timestamp()`
	if start {
		query += `,online=true,anchor_joined=false,anchor_trans_lang='',anchor_away=false,anchor_away_until=NULL,live_show=false,
			live_id=nextval('room_live_id_seq'),live_started_at=statement_timestamp(),live_ended_at=NULL,` + advanceHistoryBoundary
	}
	return scanRoom(r.tx.QueryRow(r.ctx, query+" WHERE id=$1 RETURNING "+roomFields,
		r.id, room.Title, replacePassword, hash, room.TranslationLanguage, room.Banner, room.Tag,
		room.Description, room.OfflineInfo, cmp.Or(string(room.Plan), "[]"), cmp.Or(string(room.Gifts), "[]"), room.TranslationEnabled))
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
	var endedAt time.Time
	if err := r.tx.QueryRow(r.ctx, `UPDATE rooms SET online=false,password_hash=NULL,anchor_joined=false,anchor_trans_lang='',anchor_away=false,anchor_away_until=NULL,live_show=false,
		live_ended_at=statement_timestamp(),updated_at=statement_timestamp(),`+advanceHistoryBoundary+
		" WHERE id=$1 RETURNING live_ended_at", r.id).Scan(&endedAt); err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE room_broadcast_metrics SET ended_at=$2 WHERE live_id=$1`, room.Session.LiveID, endedAt); err != nil {
		return err
	}
	minutes := int64(endedAt.Sub(room.Session.StartedAt) / time.Minute)
	if room.Session.LiveID > 0 && !room.Session.StartedAt.IsZero() && minutes > 0 {
		if err := recordAnchorTask(r.ctx, r.tx, actor, endedAt, taskEvent{target: anchor.TaskLiveMinutes, key: strconv.FormatInt(room.Session.LiveID, 10), count: minutes}); err != nil {
			return err
		}
	}
	if _, err = r.tx.Exec(r.ctx, "DELETE FROM room_members WHERE room_id=$1", r.id); err != nil {
		return err
	}
	if _, err = r.tx.Exec(r.ctx, "DELETE FROM room_viewer_nids WHERE room_id=$1", r.id); err != nil {
		return err
	}
	if _, err = r.tx.Exec(r.ctx, "DELETE FROM room_anchor_connections WHERE room_id=$1", r.id); err != nil {
		return err
	}
	_, err = r.tx.Exec(r.ctx, "DELETE FROM room_password_attempts WHERE room_id=$1", r.id)
	r.event = live.Event{Type: "close", UserID: actor, CreatedAt: endedAt}
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

func (r *roomChange) memberLiveID(uid int64) (int64, error) {
	var liveID int64
	var online, member bool
	err := r.tx.QueryRow(r.ctx, `SELECT live_id, online, id=$2 OR EXISTS(
		SELECT 1 FROM room_members WHERE room_id=$1 AND user_id=$2
	) FROM rooms WHERE id=$1`, r.id, uid).Scan(&liveID, &online, &member)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, live.ErrRoomNotFound
	case err != nil:
		return 0, err
	case !online:
		return 0, live.ErrRoomClosed
	case !member:
		return 0, live.ErrNotMember
	}
	return liveID, nil
}

func (s *Store) Join(ctx context.Context, id, uid int64, password string) error {
	_, err := s.joinRoom(ctx, id, uid, password, nil, false)
	return err
}

func (s *Store) JoinRoom(ctx context.Context, id, uid int64, password string) (live.JoinedRoom, error) {
	return s.joinRoom(ctx, id, uid, password, nil, true)
}

func (s *Store) JoinRoomWithLanguage(ctx context.Context, id, uid int64, password, language string) (live.JoinedRoom, error) {
	return s.joinRoom(ctx, id, uid, password, &language, true)
}

func (s *Store) joinRoom(ctx context.Context, id, uid int64, password string, language *string, includeMembers bool) (live.JoinedRoom, error) {
	var result live.JoinedRoom
	var rejectedPassword bool
	err := s.change(ctx, id, func(r *roomChange) error {
		var owner int64
		var online, kicked, anchorAway bool
		var hash []byte
		var failedAttempts int
		err := r.tx.QueryRow(ctx, `SELECT owner_id, online, password_hash, anchor_joined, anchor_away,
			EXISTS(SELECT 1 FROM room_kicks WHERE room_id=$1 AND user_id=$2),
			COALESCE((SELECT failed_attempts FROM room_password_attempts WHERE room_id=$1 AND user_id=$2), 0)
			FROM rooms WHERE id=$1`, id, uid).Scan(&owner, &online, &hash, &result.AnchorJoined, &anchorAway, &kicked, &failedAttempts)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return live.ErrRoomNotFound
		case err != nil:
			return err
		case !online:
			return live.ErrRoomClosed
		case uid == owner:
			before, err := r.translationLanguages()
			if err != nil {
				return err
			}
			if _, err := r.tx.Exec(ctx, "UPDATE rooms SET anchor_joined=true, anchor_trans_lang=COALESCE(NULLIF(trans_lang,''),'en') WHERE id=$1", id); err != nil {
				return err
			}
			result.AnchorJoined = true
			if anchorAway {
				if err := s.setAnchorAway(r, owner, false); err != nil {
					return err
				}
			}
			result.Languages, err = r.recordTranslationChange(before)
			if err != nil {
				return err
			}
			return r.readJoinResult(&result, uid, includeMembers)
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
		before, err := r.translationLanguages()
		if err != nil {
			return err
		}
		joined, err := r.relation("room_members", uid, true)
		if err != nil {
			return err
		}
		if joined {
			r.event = live.Event{Type: "join", UserID: uid}
			if err := r.updateMaxRoomAudience(); err != nil {
				return err
			}
			if err := r.queueBotWelcome(uid); err != nil {
				return err
			}
		}
		if _, err := r.tx.Exec(ctx, `UPDATE room_members SET trans_lang=COALESCE(NULLIF(COALESCE($3,u.trans_lang),''),'en')
			FROM users u WHERE room_members.room_id=$1 AND room_members.user_id=$2 AND u.id=$2`, id, uid, language); err != nil {
			return err
		}
		if _, err := r.tx.Exec(ctx, "UPDATE users SET current_viewer_room=$1 WHERE id=$2", id, uid); err != nil {
			return err
		}
		result.Languages, err = r.recordTranslationChange(before)
		if err != nil {
			return err
		}
		return r.readJoinResult(&result, uid, includeMembers)
	})
	// Return the credential error after committing the failure count.
	if err == nil && rejectedPassword {
		return live.JoinedRoom{}, live.ErrInvalidRoomPassword
	}
	if err != nil {
		return live.JoinedRoom{}, err
	}
	return result, nil
}

func (r *roomChange) readJoinResult(result *live.JoinedRoom, uid int64, includeMembers bool) error {
	room, err := scanRoom(r.tx.QueryRow(r.ctx, roomQuery+" WHERE id=$1", r.id))
	if err != nil {
		return err
	}
	result.Room = room
	if uid == room.OwnerID {
		result.ViewerNID = room.Session.AnchorNID
	} else {
		result.ViewerNID, err = r.viewerNID(room.Session.LiveID, uid)
		if err != nil {
			return err
		}
	}
	if !includeMembers {
		return nil
	}
	return r.tx.QueryRow(r.ctx, `SELECT ARRAY(
		SELECT user_id FROM room_members WHERE room_id=$1 ORDER BY user_id
	)`, r.id).Scan(&result.Members)
}

func (s *Store) RTCNID(ctx context.Context, id, uid int64) (uint32, error) {
	var nid uint32
	err := s.change(ctx, id, func(r *roomChange) error {
		var liveID, owner int64
		var online, member bool
		err := r.tx.QueryRow(ctx, `SELECT live_id, owner_id, online,
			EXISTS(SELECT 1 FROM room_members WHERE room_id=$1 AND user_id=$2)
			FROM rooms WHERE id=$1`, id, uid).Scan(&liveID, &owner, &online, &member)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return live.ErrRoomNotFound
		case err != nil:
			return err
		case !online:
			return live.ErrRoomClosed
		case uid == owner:
			nid = live.DefaultAnchorNID
			return nil
		case !member:
			return live.ErrNotMember
		}
		nid, err = r.viewerNID(liveID, uid)
		return err
	})
	if err != nil {
		return 0, err
	}
	return nid, nil
}

// viewerNID runs under the room's transaction lock, including across instances.
func (r *roomChange) viewerNID(liveID, uid int64) (uint32, error) {
	var nid uint32
	err := r.tx.QueryRow(r.ctx, `SELECT nid FROM room_viewer_nids
		WHERE room_id=$1 AND live_id=$2 AND user_id=$3`, r.id, liveID, uid).Scan(&nid)
	if !errors.Is(err, pgx.ErrNoRows) {
		return nid, err
	}
	err = r.tx.QueryRow(r.ctx, `WITH next_nid AS (
		UPDATE rooms SET last_viewer_nid=last_viewer_nid+1
		WHERE id=$1 AND live_id=$2 AND last_viewer_nid<$4
		RETURNING id, live_id, last_viewer_nid
	) INSERT INTO room_viewer_nids(room_id,live_id,user_id,nid)
	SELECT id, live_id, $3::bigint, last_viewer_nid FROM next_nid RETURNING nid`,
		r.id, liveID, uid, int64(math.MaxUint32)).Scan(&nid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, live.ErrViewerNIDExhausted
	}
	return nid, err
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
		before, err := r.translationLanguages()
		if err != nil {
			return err
		}
		left, err := r.relation("room_members", uid, false)
		if left {
			r.event = live.Event{Type: "leave", UserID: uid}
			_, err = r.recordTranslationChange(before)
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

func (s *Store) Kicked(ctx context.Context, roomID, userID int64) (bool, error) {
	var kicked bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM room_kicks WHERE room_id=$1 AND user_id=$2)`,
		roomID, userID,
	).Scan(&kicked)
	return kicked, err
}

func (s *Store) Muted(ctx context.Context, roomID, userID int64) (bool, error) {
	var muted bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM room_mutes WHERE room_id=$1 AND user_id=$2 AND expires_at>now())`,
		roomID, userID,
	).Scan(&muted)
	return muted, err
}

func (s *Store) MutedUsers(ctx context.Context, roomID int64) ([]int64, error) {
	var ids []int64
	err := s.pool.QueryRow(ctx,
		`SELECT ARRAY(
			SELECT user_id FROM room_mutes
			WHERE room_id=$1 AND expires_at>now()
			ORDER BY user_id
		) FROM rooms WHERE id=$1`,
		roomID,
	).Scan(&ids)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, live.ErrRoomNotFound
	}
	return ids, err
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
		before, err := r.translationLanguages()
		if err != nil {
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
		if err != nil {
			return err
		}
		r.event = live.Event{Type: "kick", UserID: actor, TargetID: target}
		_, err = r.recordTranslationChange(before)
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
		var err error
		messageID, err = r.sendChat(msg)
		return err
	})
	if err != nil {
		return 0, err
	}
	return messageID, nil
}

func (r *roomChange) sendChat(msg live.ChatMessage) (int64, error) {
	if _, err := r.memberLiveID(msg.UserID); err != nil {
		return 0, err
	}
	var muted bool
	if err := r.tx.QueryRow(r.ctx, `SELECT EXISTS(SELECT 1 FROM room_mutes WHERE room_id=$1 AND user_id=$2 AND expires_at>now())
		OR COALESCE((SELECT u.account_status IN (1,2) AND `+activeAccountLimit+` FROM users u WHERE u.id=$2 FOR SHARE),FALSE)`,
		r.id, msg.UserID).Scan(&muted); err != nil {
		return 0, err
	}
	if muted {
		return 0, live.ErrMuted
	}
	var messageID int64
	err := r.tx.QueryRow(r.ctx, `INSERT INTO room_messages(room_id,user_id,text,payload)
		VALUES($1,$2,$3,$4) RETURNING id`, r.id, msg.UserID, msg.Text, msg.Payload).Scan(&messageID)
	r.event = live.Event{Type: "chat", UserID: msg.UserID, MessageID: messageID}
	return messageID, err
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
	sub, err := s.SubscribeLive(ctx, id, uid)
	return sub.Events, sub.Unsubscribe, err
}

func (s *Store) SubscribeLive(ctx context.Context, id, uid int64) (live.Subscription, error) {
	return s.subscribeLive(ctx, id, uid, false)
}

func (s *Store) SubscribeWithTranslations(ctx context.Context, id, uid int64) (live.Subscription, error) {
	return s.subscribeLive(ctx, id, uid, true)
}

func (s *Store) subscribeLive(ctx context.Context, id, uid int64, translations bool) (live.Subscription, error) {
	ch := make(chan live.Event, live.EventStreamCapacity)
	var liveID int64
	err := s.change(ctx, id, func(r *roomChange) error {
		if !s.eventReady.Load() {
			return errRoomEventsUnavailable
		}
		var err error
		liveID, err = r.memberLiveID(uid)
		if err != nil {
			return err
		}
		r.sub, r.subUser = ch, uid
		r.subTranslations = translations
		return nil
	})
	if err != nil {
		return live.Subscription{}, err
	}
	unsubscribe := func() {
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
	}
	return live.Subscription{Events: ch, LiveID: liveID, Unsubscribe: unsubscribe}, nil
}
