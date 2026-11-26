package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/date"
	"open_live_platform/internal/live"
	"open_live_platform/internal/stt"
)

func (s *Store) ChangeDateHost(ctx context.Context, id int64, update func(*date.Host, *date.Session, time.Time) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		// Each participant may belong to at most one date across both roles.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(733516,30)`); err != nil {
			return err
		}
		host, session, err := loadDateHost(ctx, tx, id)
		if err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return err
		}
		previous := session
		if err := update(&host, &session, now); err != nil {
			return err
		}
		if session.Channel != 0 && session.EndedAt.IsZero() && session.Channel != previous.Channel {
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM date_sessions WHERE ended_at IS NULL
				AND (anchor_id IN ($1,$2) OR audience_id IN ($1,$2)))`, session.AnchorID, session.AudienceID).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return date.ErrBusy
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO date_hosts(anchor_id,channel,state) VALUES ($1,$2,$3)
			ON CONFLICT(anchor_id) DO UPDATE SET channel=EXCLUDED.channel,state=EXCLUDED.state`, id, host.Channel, host); err != nil {
			return err
		}
		if session.Channel == 0 || session == previous {
			return nil
		}
		var end *time.Time
		if !session.EndedAt.IsZero() {
			end = &session.EndedAt
		}
		if previous.Channel == session.Channel {
			_, err := tx.Exec(ctx, `UPDATE date_sessions SET ended_at=$2,state=$3 WHERE channel=$1`, session.Channel, end, session)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO date_sessions(channel,anchor_id,audience_id,ended_at,state)
			VALUES ($1,$2,$3,$4,$5)`, session.Channel, session.AnchorID, session.AudienceID, end, session)
		return err
	})
}

type dateQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadDateHost(ctx context.Context, q dateQueryer, id int64) (date.Host, date.Session, error) {
	var host date.Host
	var session date.Session
	var hostJSON, sessionJSON []byte
	err := q.QueryRow(ctx, `SELECT h.state,s.state FROM date_hosts h LEFT JOIN date_sessions s ON s.channel=h.channel WHERE h.anchor_id=$1`, id).
		Scan(&hostJSON, &sessionJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return host, session, nil
	}
	if err != nil {
		return host, session, err
	}
	if err = json.Unmarshal(hostJSON, &host); err == nil && sessionJSON != nil {
		err = json.Unmarshal(sessionJSON, &session)
	}
	return host, session, err
}

func (s *Store) DateHost(ctx context.Context, id int64) (date.Host, date.Session, error) {
	return loadDateHost(ctx, s.pool, id)
}

func loadDateSession(ctx context.Context, q dateQueryer, channel int64) (date.Session, error) {
	var session date.Session
	err := q.QueryRow(ctx, `SELECT state FROM date_sessions WHERE channel=$1`, channel).Scan(&session)
	if errors.Is(err, pgx.ErrNoRows) {
		return session, date.ErrNotFound
	}
	return session, err
}

func (s *Store) DateSession(ctx context.Context, channel int64) (date.Session, error) {
	return loadDateSession(ctx, s.pool, channel)
}

func (s *Store) ActiveDateHosts(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT h.anchor_id FROM date_hosts h LEFT JOIN date_sessions s ON s.channel=h.channel
		WHERE h.state->'Pending'<>'null'::jsonb OR (s.channel IS NOT NULL AND s.ended_at IS NULL)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

type dateSTT struct{ pg *Store }

func (s *Store) DateSubtitles() stt.Store { return dateSTT{s} }

func (s dateSTT) ChangeSTTTask(ctx context.Context, channel int64, change func(stt.RoomState, *stt.Task) error) error {
	return pgx.BeginTxFunc(ctx, s.pg.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(733517,hashint8($1::bigint))`, channel); err != nil {
			return err
		}
		session, err := loadDateSession(ctx, tx, channel)
		if errors.Is(err, date.ErrNotFound) {
			return live.ErrRoomNotFound
		}
		if err != nil {
			return err
		}
		var task stt.Task
		err = tx.QueryRow(ctx, `SELECT state FROM date_stt_tasks WHERE channel=$1`, channel).Scan(&task)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		original := task
		room := stt.RoomState{OwnerID: session.AnchorID, ParticipantID: session.AudienceID, LiveID: channel, Online: session.Active(time.Now())}
		if err := change(room, &task); err != nil || task == original {
			return err
		}
		if task.RemoteID == "" {
			_, err = tx.Exec(ctx, `DELETE FROM date_stt_tasks WHERE channel=$1`, channel)
			return err
		}
		current, err := loadDateSession(ctx, tx, channel)
		if err != nil {
			return err
		}
		if !current.Active(time.Now()) {
			return date.ErrClosed
		}
		_, err = tx.Exec(ctx, `INSERT INTO date_stt_tasks(channel,state) VALUES($1,$2)
			ON CONFLICT(channel) DO UPDATE SET state=EXCLUDED.state`, channel, task)
		return err
	})
}

func (s dateSTT) StaleSTTTaskRooms(ctx context.Context) ([]int64, error) {
	rows, err := s.pg.pool.Query(ctx, `SELECT t.channel FROM date_stt_tasks t JOIN date_sessions s ON s.channel=t.channel
		WHERE s.ended_at IS NOT NULL OR (s.state->>'AnchorSeen')::timestamptz<=statement_timestamp()-INTERVAL '10 seconds'
		OR (s.state->>'AudienceSeen')::timestamptz<=statement_timestamp()-INTERVAL '10 seconds'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
