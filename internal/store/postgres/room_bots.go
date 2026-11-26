package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
	"open_live_platform/internal/livebot"
)

const roomBotBatch = 64

type roomBots struct {
	config livebot.Config
	ids    []int64
}

// ConfigureRoomBots publishes a validated, immutable configuration to room updates.
func (s *Store) ConfigureRoomBots(config livebot.Config) error {
	ids, err := config.AccountIDs()
	if err != nil {
		return err
	}
	s.bots.Store(&roomBots{config: config, ids: ids})
	return nil
}

func (r *roomChange) queueBotWelcome(uid int64) error {
	if r.bots == nil {
		return nil
	}
	return r.queueBotReply(uid, nil)
}

func (r *roomChange) queueBotGift(uid int64, points int64, giftType int8) error {
	if r.bots == nil || points < r.bots.config.MinimumGiftPoints {
		return nil
	}
	return r.queueBotReply(uid, &giftType)
}

func (r *roomChange) queueBotReply(sender int64, giftType *int8) error {
	var botID int64
	err := r.tx.QueryRow(r.ctx, `SELECT b.user_id FROM room_bot_members b JOIN rooms r ON r.id=b.room_id
		WHERE b.room_id=$1 AND b.user_id<>$2 AND b.live_id=r.live_id AND b.expires_at>statement_timestamp()
		AND r.online AND r.password_hash IS NULL
		AND ($3::boolean OR EXISTS(SELECT 1 FROM nft_bindings WHERE user_id=$2))`,
		r.id, sender, giftType != nil).Scan(&botID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	text, delay := r.bots.config.Reply(botID, giftType)
	if text == "" {
		return nil
	}
	_, err = r.tx.Exec(r.ctx, `INSERT INTO room_bot_replies(room_id,text,send_at)
		VALUES($1,$2,statement_timestamp()+$3::interval)`, r.id, text, delay)
	return err
}

const botAvailable = ` (u.account_status NOT IN (1,2) OR NOT ` + activeAccountLimit + `)
	AND NOT EXISTS(SELECT 1 FROM room_bot_members WHERE user_id=u.id)
	AND NOT EXISTS(SELECT 1 FROM rooms WHERE owner_id=u.id AND online)
	AND NOT EXISTS(SELECT 1 FROM room_members WHERE user_id=u.id) `

const botEligibleRoom = ` r.online AND NOT r.anchor_away AND r.password_hash IS NULL
	AND (SELECT count(*) FROM room_members WHERE room_id=r.id)<$2
	AND r.live_started_at+$3::interval<=statement_timestamp() `

func (s *Store) SweepRoomBots(ctx context.Context) error {
	bots := s.bots.Load()
	if bots == nil {
		return nil
	}
	maxPopulation, minEntry := bots.config.CandidateSchedule()
	entryDelay := time.Duration(minEntry) * time.Second
	rows, err := s.pool.Query(ctx, `SELECT room_id FROM (
		SELECT room_id,send_at AS due FROM room_bot_replies WHERE send_at<=statement_timestamp()
		UNION ALL
		SELECT b.room_id,b.like_at FROM room_bot_members b WHERE b.like_at<=statement_timestamp()
		UNION ALL
		SELECT b.room_id,b.daily_at FROM room_bot_members b WHERE $6 AND b.daily_at<=statement_timestamp()
		UNION ALL
		SELECT b.room_id,b.expires_at FROM room_bot_members b JOIN rooms r ON r.id=b.room_id JOIN users u ON u.id=b.user_id
			WHERE b.expires_at<=statement_timestamp() OR NOT r.online OR r.live_id<>b.live_id
				OR r.password_hash IS NOT NULL OR (u.account_status IN (1,2) AND `+activeAccountLimit+`)
				OR NOT CASE WHEN $5 THEN EXISTS(SELECT 1 FROM room_bot_pool p WHERE p.user_id=b.user_id)
					ELSE b.user_id=ANY($1::bigint[]) END
		UNION ALL
		SELECT r.id,r.live_started_at+$3::interval FROM rooms r
			WHERE `+botEligibleRoom+` AND NOT EXISTS(SELECT 1 FROM room_bot_members WHERE room_id=r.id)
			AND EXISTS(SELECT 1 FROM users u WHERE
				CASE WHEN $5 THEN EXISTS(SELECT 1 FROM room_bot_pool p WHERE p.user_id=u.id) ELSE u.id=ANY($1::bigint[]) END
				AND `+botAvailable+`
				AND NOT EXISTS(SELECT 1 FROM room_kicks WHERE room_id=r.id AND user_id=u.id))
		) AS pending GROUP BY room_id ORDER BY min(due),room_id LIMIT $4`,
		bots.ids, maxPopulation, entryDelay, roomBotBatch, bots.config.DatabasePool, bots.config.DailyChat)
	if err != nil {
		return err
	}
	roomIDs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, roomID := range roomIDs {
		if err := s.change(ctx, roomID, func(r *roomChange) error { return r.sweepRoomBot() }); err != nil {
			return err
		}
	}
	return nil
}

func (r *roomChange) sweepRoomBot() error {
	var botID int64
	var departing bool
	err := r.tx.QueryRow(r.ctx, `SELECT b.user_id, NOT r.online OR r.live_id<>b.live_id OR r.password_hash IS NOT NULL
		OR b.expires_at<=statement_timestamp() OR (u.account_status IN (1,2) AND `+activeAccountLimit+`)
		OR NOT CASE WHEN $3 THEN EXISTS(SELECT 1 FROM room_bot_pool p WHERE p.user_id=b.user_id)
			ELSE b.user_id=ANY($2::bigint[]) END
		FROM room_bot_members b JOIN rooms r ON r.id=b.room_id JOIN users u ON u.id=b.user_id
		WHERE b.room_id=$1`, r.id, r.bots.ids, r.bots.config.DatabasePool).Scan(&botID, &departing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return r.addRoomBot()
	case err != nil:
		return err
	case departing:
		left, err := r.relation("room_members", botID, false)
		if err == nil && left {
			r.event = live.Event{Type: "leave", UserID: botID}
		}
		return err
	default:
		return r.runBotAction(botID)
	}
}

func (r *roomChange) addRoomBot() error {
	bots := r.bots
	var liveID int64
	if err := r.tx.QueryRow(r.ctx, `SELECT live_id FROM rooms WHERE id=$1`, r.id).Scan(&liveID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	population, entry, stay := bots.config.RoomSchedule(liveID)
	var botID int64
	err := r.tx.QueryRow(r.ctx, `INSERT INTO room_bot_members(room_id,user_id,live_id,expires_at)
		SELECT r.id,u.id,r.live_id,statement_timestamp()+$4::interval
		FROM rooms r CROSS JOIN users u WHERE r.id=$5 AND `+botEligibleRoom+`
			AND CASE WHEN $6 THEN EXISTS(SELECT 1 FROM room_bot_pool p WHERE p.user_id=u.id) ELSE u.id=ANY($1::bigint[]) END
			AND `+botAvailable+`
			AND NOT EXISTS(SELECT 1 FROM room_kicks WHERE room_id=r.id AND user_id=u.id)
		ORDER BY u.id LIMIT 1 FOR UPDATE OF u SKIP LOCKED ON CONFLICT DO NOTHING RETURNING user_id`, bots.ids, population,
		time.Duration(entry)*time.Second, time.Duration(stay)*time.Second, r.id, bots.config.DatabasePool).Scan(&botID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := r.relation("room_members", botID, true); err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `UPDATE room_bot_members SET like_at=statement_timestamp()+$2::interval,
		daily_at=CASE WHEN $3 THEN statement_timestamp()+interval '5 seconds' END WHERE room_id=$1`,
		r.id, bots.config.LikeDelay(liveID, botID), bots.config.DailyChat); err != nil {
		return err
	}
	if err := r.updateMaxRoomAudience(); err != nil {
		return err
	}
	r.event = live.Event{Type: "join", UserID: botID}
	return nil
}

func (r *roomChange) runBotAction(botID int64) error {
	var kind string
	err := r.tx.QueryRow(r.ctx, `SELECT kind FROM (
		SELECT 'reply' AS kind,min(send_at) AS due FROM room_bot_replies WHERE room_id=$1
		UNION ALL SELECT 'like',like_at FROM room_bot_members WHERE room_id=$1
		UNION ALL SELECT 'daily',daily_at FROM room_bot_members WHERE room_id=$1 AND $2
	) AS actions WHERE due<=statement_timestamp() ORDER BY due,kind LIMIT 1`, r.id, r.bots.config.DailyChat).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch kind {
	case "reply":
		return r.sendBotReply(botID)
	case "like":
		var liveID int64
		err := r.tx.QueryRow(r.ctx, `UPDATE room_bot_members SET like_at=NULL
			WHERE room_id=$1 AND expires_at>statement_timestamp() RETURNING live_id`, r.id).Scan(&liveID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = r.addRoomLikes(liveID, botID, 1)
		return err
	case "daily":
		text, delay := r.bots.config.DailyReply(botID)
		tag, err := r.tx.Exec(r.ctx, `UPDATE room_bot_members SET daily_at=statement_timestamp()+$2::interval
			WHERE room_id=$1 AND expires_at>statement_timestamp()`, r.id, delay)
		if err != nil || text == "" || tag.RowsAffected() == 0 {
			return err
		}
		return r.sendBotChat(botID, text)
	default:
		return nil
	}
}

func (r *roomChange) sendBotReply(botID int64) error {
	var text string
	err := r.tx.QueryRow(r.ctx, `DELETE FROM room_bot_replies WHERE id=(
		SELECT id FROM room_bot_replies WHERE room_id=$1 AND send_at<=statement_timestamp()
		ORDER BY send_at,id LIMIT 1) RETURNING text`, r.id).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.sendBotChat(botID, text)
}

func (r *roomChange) sendBotChat(botID int64, text string) error {
	_, err := r.sendChat(live.ChatMessage{RoomID: r.id, UserID: botID, Text: text})
	if errors.Is(err, live.ErrMuted) || errors.Is(err, live.ErrKicked) {
		return nil
	}
	return err
}
