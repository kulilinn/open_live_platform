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
		SELECT b.room_id,b.expires_at FROM room_bot_members b JOIN rooms r ON r.id=b.room_id
			WHERE b.expires_at<=statement_timestamp() OR NOT r.online OR r.live_id<>b.live_id
				OR r.password_hash IS NOT NULL OR NOT (b.user_id=ANY($1::bigint[]))
		UNION ALL
		SELECT r.id,r.live_started_at+$3::interval FROM rooms r
			WHERE `+botEligibleRoom+` AND NOT EXISTS(SELECT 1 FROM room_bot_members WHERE room_id=r.id)
			AND EXISTS(SELECT 1 FROM users u WHERE u.id=ANY($1::bigint[]) AND `+botAvailable+`
				AND NOT EXISTS(SELECT 1 FROM room_kicks WHERE room_id=r.id AND user_id=u.id))
		) AS pending GROUP BY room_id ORDER BY min(due),room_id LIMIT $4`,
		bots.ids, maxPopulation, entryDelay, roomBotBatch)
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
		OR b.expires_at<=statement_timestamp() OR NOT (b.user_id=ANY($2::bigint[]))
		FROM room_bot_members b JOIN rooms r ON r.id=b.room_id WHERE b.room_id=$1`, r.id, r.bots.ids).Scan(&botID, &departing)
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
		return r.sendBotReply(botID)
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
			AND u.id=ANY($1::bigint[]) AND `+botAvailable+`
			AND NOT EXISTS(SELECT 1 FROM room_kicks WHERE room_id=r.id AND user_id=u.id)
		ORDER BY u.id LIMIT 1 ON CONFLICT DO NOTHING RETURNING user_id`, bots.ids, population,
		time.Duration(entry)*time.Second, time.Duration(stay)*time.Second, r.id).Scan(&botID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := r.relation("room_members", botID, true); err != nil {
		return err
	}
	if err := r.updateMaxRoomAudience(); err != nil {
		return err
	}
	r.event = live.Event{Type: "join", UserID: botID}
	return nil
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
	_, err = r.sendChat(live.ChatMessage{RoomID: r.id, UserID: botID, Text: text})
	if errors.Is(err, live.ErrMuted) || errors.Is(err, live.ErrKicked) {
		return nil
	}
	return err
}
