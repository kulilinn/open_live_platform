package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/liveaudit"
)

func (s *Store) ClaimLiveMediaAudit(ctx context.Context) (liveaudit.Task, bool, error) {
	var task liveaudit.Task
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO live_media_audit_tasks(live_id,room_id,anchor_id,anchor_name)
			SELECT r.live_id,r.id,r.owner_id,u.account FROM rooms r JOIN users u ON u.id=r.owner_id
			WHERE r.online AND r.live_id>0 ON CONFLICT DO NOTHING`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `WITH item AS (
			SELECT t.live_id,(t.request_id IS NOT NULL) AS stop
			FROM live_media_audit_tasks t JOIN rooms r ON r.id=t.room_id
			WHERE t.available_at<=statement_timestamp()
			AND ((t.request_id IS NULL AND ((r.online AND r.live_id=t.live_id) OR t.opening))
				OR (t.request_id IS NOT NULL AND (t.retry OR NOT r.online OR r.live_id<>t.live_id)))
			AND (t.request_id IS NOT NULL OR NOT EXISTS(SELECT 1 FROM live_media_audit_tasks older
				WHERE older.room_id=t.room_id AND older.live_id<t.live_id AND (older.request_id IS NOT NULL OR older.opening)))
			ORDER BY t.available_at,t.live_id LIMIT 1 FOR UPDATE OF t SKIP LOCKED)
		UPDATE live_media_audit_tasks t SET available_at=statement_timestamp()+interval '12 seconds',version=t.version+1,opening=NOT item.stop
		FROM item WHERE t.live_id=item.live_id RETURNING t.room_id,t.live_id,t.anchor_id,t.anchor_name,COALESCE(t.request_id,''),t.version,item.stop`).
			Scan(&task.RoomID, &task.LiveID, &task.OwnerID, &task.OwnerName, &task.RequestID, &task.Version, &task.Stop)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, nil
	}
	return task, err == nil, err
}

func (s *Store) FinishLiveMediaAudit(ctx context.Context, task liveaudit.Task, requestID string, failed bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var version int64
		if err := tx.QueryRow(ctx, `SELECT version FROM live_media_audit_tasks WHERE live_id=$1 FOR UPDATE`, task.LiveID).Scan(&version); err != nil {
			return err
		}
		if version != task.Version {
			return nil
		}
		if failed {
			_, err := tx.Exec(ctx, `UPDATE live_media_audit_tasks SET failures=LEAST(failures+1,5),
				available_at=statement_timestamp()+make_interval(secs=>least(30,(1<<failures))) WHERE live_id=$1`, task.LiveID)
			return err
		}
		if task.Stop {
			if _, err := tx.Exec(ctx, `UPDATE live_media_audit_requests SET stopped_at=statement_timestamp() WHERE request_id=$1`, task.RequestID); err != nil {
				return err
			}
		}
		if requestID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO live_media_audit_requests(request_id,live_id) VALUES($1,$2)`, requestID, task.LiveID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE live_media_audit_tasks SET request_id=NULLIF($2,''),retry=false,opening=false,failures=0,
			available_at=statement_timestamp() WHERE live_id=$1`, task.LiveID, requestID)
		return err
	})
}

func (s *Store) ApplyLiveMediaAudit(ctx context.Context, decision liveaudit.Decision) (liveaudit.Task, bool, error) {
	var task liveaudit.Task
	err := s.pool.QueryRow(ctx, `SELECT t.room_id,t.live_id,t.anchor_id FROM live_media_audit_requests p
		JOIN live_media_audit_tasks t ON t.live_id=p.live_id WHERE p.request_id=$1`, decision.ProviderID).
		Scan(&task.RoomID, &task.LiveID, &task.OwnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return task, false, liveaudit.ErrUnknown
	}
	if err != nil {
		return task, false, err
	}
	if decision.Identity.Channel != strconv.FormatInt(task.RoomID, 10) || decision.Identity.AnchorID != strconv.FormatInt(task.OwnerID, 10) {
		return task, false, liveaudit.ErrUnknown
	}
	var applied bool
	err = s.change(ctx, task.RoomID, func(r *roomChange) error {
		var current bool
		if err := r.tx.QueryRow(ctx, `SELECT r.online AND r.live_id=t.live_id AND COALESCE(t.request_id=$1,false) FROM rooms r
			JOIN live_media_audit_tasks t ON t.room_id=r.id WHERE t.live_id=$2 FOR UPDATE OF t`, decision.ProviderID, task.LiveID).Scan(&current); err != nil {
			return err
		}
		tag, err := r.tx.Exec(ctx, `INSERT INTO live_media_audit_history(request_id,callback_key,kind,payload,applied)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, decision.ProviderID, decision.EventID, decision.Kind, decision.Payload, current && decision.Operation > 0)
		if err != nil || tag.RowsAffected() == 0 || !current {
			return err
		}
		if decision.Retry {
			_, err := r.tx.Exec(ctx, `UPDATE live_media_audit_tasks SET retry=true,available_at=statement_timestamp()+interval '5 seconds'
				WHERE live_id=$1`, task.LiveID)
			return err
		}
		kind := map[int]string{1: "streamer_alert", 2: "no_talking", 3: "streamer_disable", 4: "account_disable"}[decision.Operation]
		if kind == "" {
			return nil
		}
		applied = true
		return s.applyBackofficeAction(ctx, r, admin.Principal{Area: admin.OperationArea, Tenant: admin.OperationTenant, Username: "Shumei live audit"},
			admin.Action{Kind: kind, Path: decision.Kind, UserID: task.OwnerID, RoomID: task.RoomID, Duration: decision.Duration, Reason: decision.Reasons})
	})
	return task, applied, err
}
