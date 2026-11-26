package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/live"
	"open_live_platform/internal/stt"
)

func (s *Store) ChangeSTTTask(ctx context.Context, roomID int64, change func(stt.RoomState, *stt.Task) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var room stt.RoomState
		err := tx.QueryRow(ctx, `SELECT owner_id,live_id,online FROM rooms WHERE id=$1 FOR UPDATE`, roomID).
			Scan(&room.OwnerID, &room.LiveID, &room.Online)
		if errors.Is(err, pgx.ErrNoRows) {
			return live.ErrRoomNotFound
		}
		if err != nil {
			return err
		}
		var task stt.Task
		err = tx.QueryRow(ctx, `SELECT room_id,live_id,api_version,remote_id,builder_token,sequence FROM room_stt_tasks WHERE room_id=$1`, roomID).
			Scan(&task.RoomID, &task.LiveID, &task.Version, &task.RemoteID, &task.BuilderToken, &task.Sequence)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		previous := task
		if err := change(room, &task); err != nil || task == previous {
			return err
		}
		if task.RemoteID == "" {
			_, err = tx.Exec(ctx, `DELETE FROM room_stt_tasks WHERE room_id=$1`, roomID)
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO room_stt_tasks(room_id,live_id,api_version,remote_id,builder_token,sequence)
			VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT(room_id) DO UPDATE SET
			live_id=EXCLUDED.live_id,api_version=EXCLUDED.api_version,remote_id=EXCLUDED.remote_id,
			builder_token=EXCLUDED.builder_token,sequence=EXCLUDED.sequence`, roomID, task.LiveID, task.Version, task.RemoteID, task.BuilderToken, task.Sequence)
		return err
	})
}

func (s *Store) StaleSTTTaskRooms(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.room_id FROM room_stt_tasks t JOIN rooms r ON r.id=t.room_id
		WHERE NOT r.online OR t.live_id<>r.live_id ORDER BY t.room_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
