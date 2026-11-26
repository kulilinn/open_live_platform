package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/dm"
	"open_live_platform/internal/notice"
)

type LegacyDMConversation struct {
	ID                      int64
	Type                    int
	Deleted                 bool
	Members, Hidden         []int64
	Metadata                dm.Metadata
	LastMessage             string
	LastActivity, CreatedAt time.Time
	CanMessage              bool
	EntitlementUntil        *time.Time
}

type LegacyDMMessage struct {
	ID, ConversationID   int64
	SenderID, ReceiverID int64
	Group, Deleted       bool
	Type, Status         int
	Content              string
	CreatedAt            time.Time
}

type LegacyNotice struct {
	notice.Notice
	Deleted bool
}

type InteractionImportCounts struct {
	Conversations, Messages, Notices int
	Existing, Skipped                int
}

// ImportLegacyInteractions imports a read-only source snapshot without replacing changed target data.
func (s *Store) ImportLegacyInteractions(ctx context.Context, conversations iter.Seq2[LegacyDMConversation, error],
	messages iter.Seq2[LegacyDMMessage, error], notices iter.Seq2[LegacyNotice, error]) (InteractionImportCounts, error) {
	var counts InteractionImportCounts
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE dm_conversations,dm_messages,notices IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		for row, err := range conversations {
			if err != nil {
				return err
			}
			if err := importLegacyConversation(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("import conversation %d: %w", row.ID, err)
			}
		}
		for row, err := range messages {
			if err != nil {
				return err
			}
			if err := importLegacyDMMessage(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("import DM message %d: %w", row.ID, err)
			}
		}
		for row, err := range notices {
			if err != nil {
				return err
			}
			if err := importLegacyNotice(ctx, tx, row, &counts); err != nil {
				return fmt.Errorf("import notice %d: %w", row.ID, err)
			}
		}
		_, err := tx.Exec(ctx, `
			SELECT setval('dm_conversations_id_seq', highest.id, true)
			FROM (SELECT max(id) AS id FROM dm_conversations) highest, dm_conversations_id_seq seq
			WHERE highest.id>seq.last_value OR (highest.id=seq.last_value AND NOT seq.is_called);
			SELECT setval('dm_messages_id_seq', highest.id, true)
			FROM (SELECT max(id) AS id FROM dm_messages) highest, dm_messages_id_seq seq
			WHERE highest.id>seq.last_value OR (highest.id=seq.last_value AND NOT seq.is_called)`)
		return err
	})
	if err != nil {
		return InteractionImportCounts{}, err
	}
	return counts, nil
}

func importLegacyConversation(ctx context.Context, tx pgx.Tx, row LegacyDMConversation, counts *InteractionImportCounts) error {
	if row.Deleted || row.Type != 1 {
		counts.Skipped++
		return nil
	}
	fanID, anchorID := row.Metadata.Fan.ID, row.Metadata.Anchor.ID
	if row.ID < 1 || fanID < 1 || anchorID < 1 || fanID == anchorID || len(row.Members) != 2 ||
		!slices.Contains(row.Members, fanID) || !slices.Contains(row.Members, anchorID) || row.CreatedAt.IsZero() || row.LastActivity.IsZero() {
		return errors.New("invalid direct conversation participants or timestamps")
	}
	active := row.CanMessage && row.EntitlementUntil != nil && row.EntitlementUntil.After(time.Now())
	tag, err := tx.Exec(ctx, `INSERT INTO dm_conversations(id,fan_id,anchor_id,metadata,last_message,last_message_time,create_time,
		can_message,hidden_by_fan,hidden_by_anchor,subscription_expires_at,livemedia_imported)
		SELECT $1,f.id,a.id,$4,$5,$6,$7,$8,$9,$10,$11,TRUE FROM users f,users a WHERE f.id=$2 AND a.id=$3
		ON CONFLICT DO NOTHING`, row.ID, fanID, anchorID, row.Metadata, row.LastMessage, row.LastActivity, row.CreatedAt,
		active, slices.Contains(row.Hidden, fanID), slices.Contains(row.Hidden, anchorID), row.EntitlementUntil)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		counts.Conversations++
		return nil
	}
	var existingID int64
	var existing bool
	err = tx.QueryRow(ctx, `SELECT id,livemedia_imported AND fan_id=$2 AND anchor_id=$3 FROM dm_conversations
		WHERE id=$1 OR (member_low=LEAST($2::bigint,$3::bigint) AND member_high=GREATEST($2::bigint,$3::bigint))
		ORDER BY (id=$1) DESC LIMIT 1`, row.ID, fanID, anchorID).Scan(&existingID, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		counts.Skipped++
		return nil
	}
	if err != nil {
		return err
	}
	if !existing || existingID != row.ID {
		return errors.New("target has a different direct conversation with this ID or member pair")
	}
	counts.Existing++
	return nil
}

func importLegacyDMMessage(ctx context.Context, tx pgx.Tx, row LegacyDMMessage, counts *InteractionImportCounts) error {
	if row.Deleted || row.Group || row.ReceiverID == 0 {
		counts.Skipped++
		return nil
	}
	if row.ID < 1 || row.ConversationID < 1 || row.SenderID < 1 || row.ReceiverID < 1 || row.SenderID == row.ReceiverID ||
		row.CreatedAt.IsZero() || row.Status < dm.Pending || row.Status > dm.Read {
		return errors.New("invalid direct message identity, state or timestamp")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO dm_messages(id,conversation_id,sender_id,receiver_id,message_type,content,status,create_time,livemedia_imported)
		SELECT $1,c.id,$3,$4,$5,$6,$7,$8,TRUE FROM dm_conversations c WHERE c.id=$2 AND c.livemedia_imported
			AND c.member_low=LEAST($3::bigint,$4::bigint) AND c.member_high=GREATEST($3::bigint,$4::bigint)
		ON CONFLICT(id) DO NOTHING`, row.ID, row.ConversationID, row.SenderID, row.ReceiverID, row.Type, row.Content, row.Status, row.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		counts.Messages++
		return nil
	}
	var existing bool
	err = tx.QueryRow(ctx, `SELECT livemedia_imported AND conversation_id=$2 AND sender_id=$3 AND receiver_id=$4
		FROM dm_messages WHERE id=$1`, row.ID, row.ConversationID, row.SenderID, row.ReceiverID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		counts.Skipped++
		return nil
	}
	if err != nil {
		return err
	}
	if !existing {
		return errors.New("target direct message ID conflicts with source")
	}
	counts.Existing++
	return nil
}

func importLegacyNotice(ctx context.Context, tx pgx.Tx, row LegacyNotice, counts *InteractionImportCounts) error {
	if row.Deleted {
		counts.Skipped++
		return nil
	}
	if row.ID < 1 || row.UserID < 1 || row.CreatedAt.IsZero() || !json.Valid(row.Content) || (row.Status != 1 && row.Status != 2) {
		return errors.New("invalid notice identity, state, body or timestamp")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,status,create_time,livemedia_imported)
		SELECT $1,$2,$3,u.id,$5,$6,$7,$8,$9,TRUE FROM users u WHERE u.id=$4 ON CONFLICT(id) DO NOTHING`,
		row.ID, row.Target, row.Type, row.UserID, row.Avatar, row.Content, row.ObjectID, row.Status, row.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		counts.Notices++
		return nil
	}
	var existing bool
	err = tx.QueryRow(ctx, `SELECT livemedia_imported AND receive_id=$2 FROM notices WHERE id=$1`, row.ID, row.UserID).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		counts.Skipped++
		return nil
	}
	if err != nil {
		return err
	}
	if !existing {
		return errors.New("target notice ID conflicts with source")
	}
	counts.Existing++
	return nil
}
