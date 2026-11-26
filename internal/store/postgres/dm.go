package postgres

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/dm"
)

var _ dm.Store = (*Store)(nil)

const dmPair = `member_low=LEAST($1::bigint,$2::bigint) AND member_high=GREATEST($1::bigint,$2::bigint)`
const dmVisible = `((fan_id=$1 AND NOT hidden_by_fan) OR (anchor_id=$1 AND NOT hidden_by_anchor))`
const dmMessageColumns = `id,conversation_id,sender_id,receiver_id,message_type,content,status,create_time`
const dmCanMessage = `(can_message AND (subscription_expires_at IS NULL OR subscription_expires_at>now()))`

func (s *Store) SetDMSubscription(ctx context.Context, members dm.Metadata, active bool, welcome string) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		id, err = setDMSubscription(ctx, tx, members, active, welcome, nil)
		return err
	})
	return id, err
}

func setDMSubscription(ctx context.Context, tx pgx.Tx, members dm.Metadata, active bool, welcome string, until *time.Time) (int64, error) {
	var id int64
	fanID, anchorID := members.Fan.ID, members.Anchor.ID
	if fanID < 1 || anchorID < 1 || fanID == anchorID || (active && !members.Anchor.Anchor) {
		return 0, dm.ErrForbidden
	}
	if active {
		err := tx.QueryRow(ctx, `INSERT INTO dm_conversations(fan_id,anchor_id,metadata,subscription_expires_at)
			VALUES($1,$2,$3,$4) ON CONFLICT(member_low,member_high) DO NOTHING RETURNING id`, fanID, anchorID, members, until).Scan(&id)
		if err == nil {
			_, err = addDM(ctx, tx, dm.SendInput{ConversationID: id, SenderID: anchorID, ReceiverID: fanID,
				Type: 1, Content: cmp.Or(welcome, dm.Welcome)})
			return id, err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
	}
	err := tx.QueryRow(ctx, `UPDATE dm_conversations SET can_message=$3,subscription_expires_at=$4 WHERE `+dmPair+` RETURNING id`, fanID, anchorID, active, until).Scan(&id)
	return id, dmError(err)
}

func (s *Store) FindDMConversation(ctx context.Context, userID, otherID int64) (int64, bool, error) {
	var id int64
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT id,`+dmCanMessage+` FROM dm_conversations WHERE `+dmPair, userID, otherID).Scan(&id, &allowed)
	return id, allowed, dmError(err)
}

func (s *Store) DMConversations(ctx context.Context, userID int64, page dm.PageRequest) (dm.Page[dm.Conversation], error) {
	result := dm.Page[dm.Conversation]{Data: []dm.Conversation{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var total int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM dm_conversations WHERE `+dmVisible, userID).Scan(&total); err != nil {
			return err
		}
		offset, limit, details := page.Window(total)
		result.Pagination = details
		if limit == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT id,fan_id,anchor_id,metadata,last_message,last_message_time,create_time,`+dmCanMessage+`,
			(SELECT count(*) FROM dm_messages m WHERE m.conversation_id=c.id AND receiver_id=$1 AND status IN (1,2))
			FROM dm_conversations c WHERE `+dmVisible+` ORDER BY last_message_time DESC,id DESC LIMIT $2 OFFSET $3`, userID, limit, offset)
		if err != nil {
			return err
		}
		result.Data, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (dm.Conversation, error) {
			conversation := dm.Conversation{Type: 1}
			var fanID, anchorID int64
			err := row.Scan(&conversation.ID, &fanID, &anchorID, &conversation.Metadata, &conversation.LastMessage,
				&conversation.LastActivity, &conversation.CreatedAt, &conversation.CanMessage, &conversation.Unread)
			conversation.Members = []string{strconv.FormatInt(anchorID, 10), strconv.FormatInt(fanID, 10)}
			conversation.OtherUser = conversation.Metadata.Fan
			if userID == fanID {
				conversation.OtherUser = conversation.Metadata.Anchor
			}
			conversation.LastActivity = conversation.LastActivity.UTC()
			conversation.CreatedAt = conversation.CreatedAt.UTC()
			return conversation, err
		})
		return err
	})
	return result, err
}

func (s *Store) DMMessages(ctx context.Context, userID, conversationID int64, page dm.PageRequest) (dm.Page[dm.Message], error) {
	result := dm.Page[dm.Message]{Data: []dm.Message{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var total int64
		err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM dm_messages WHERE conversation_id=$2)
			FROM dm_conversations WHERE id=$2 AND $1 IN (fan_id,anchor_id)`, userID, conversationID).Scan(&total)
		if err != nil {
			return dmError(err)
		}
		offset, limit, details := page.Window(total)
		result.Pagination = details
		if limit == 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT `+dmMessageColumns+` FROM dm_messages WHERE conversation_id=$1
			ORDER BY create_time DESC,id DESC LIMIT $2 OFFSET $3`, conversationID, limit, offset)
		if err != nil {
			return err
		}
		result.Data, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (dm.Message, error) { return scanDMMessage(row) })
		return err
	})
	return result, err
}

func (s *Store) UnreadDM(ctx context.Context, userID int64) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dm_messages WHERE receiver_id=$1 AND status IN (1,2)`, userID).Scan(&count)
	return count, err
}

func (s *Store) ReadDM(ctx context.Context, userID, conversationID int64) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM dm_conversations WHERE id=$2 AND $1 IN (fan_id,anchor_id) FOR UPDATE`, userID, conversationID).Scan(&id)
		if err != nil {
			return dmError(err)
		}
		_, err = tx.Exec(ctx, `UPDATE dm_messages SET status=3 WHERE conversation_id=$2 AND receiver_id=$1 AND status IN (1,2)`, userID, id)
		return err
	})
}

func (s *Store) HideDM(ctx context.Context, userID, conversationID int64) error {
	result, err := s.pool.Exec(ctx, `UPDATE dm_conversations SET hidden_by_fan=hidden_by_fan OR fan_id=$1,
		hidden_by_anchor=hidden_by_anchor OR anchor_id=$1 WHERE id=$2 AND $1 IN (fan_id,anchor_id)`, userID, conversationID)
	if err == nil && result.RowsAffected() == 0 {
		return dm.ErrNotFound
	}
	return err
}

func (s *Store) CheckDMSend(ctx context.Context, input dm.SendInput) error {
	return checkDMSend(s.pool.QueryRow(ctx, selectDMSend, input.SenderID, input.ConversationID), input.ReceiverID)
}

func (s *Store) SendDM(ctx context.Context, input dm.SendInput) (dm.Message, error) {
	var result dm.Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := checkDMSend(tx.QueryRow(ctx, selectDMSend+" FOR UPDATE", input.SenderID, input.ConversationID), input.ReceiverID); err != nil {
			return err
		}
		var err error
		result, err = addDM(ctx, tx, input)
		return err
	})
	return result, err
}

const selectDMSend = `SELECT (CASE WHEN fan_id=$1 THEN anchor_id ELSE fan_id END), ` + dmCanMessage + ` AND
	NOT COALESCE((SELECT u.account_status IN (1,2) AND ` + activeAccountLimit + ` FROM users u WHERE u.id=$1 FOR SHARE),TRUE)
	FROM dm_conversations WHERE id=$2 AND $1 IN (fan_id,anchor_id)`

func checkDMSend(row pgx.Row, receiverID int64) error {
	var otherID int64
	var allowed bool
	if err := row.Scan(&otherID, &allowed); err != nil {
		return dmError(err)
	}
	if !allowed || otherID != receiverID {
		return dm.ErrForbidden
	}
	return nil
}

func addDM(ctx context.Context, tx pgx.Tx, input dm.SendInput) (dm.Message, error) {
	message, err := scanDMMessage(tx.QueryRow(ctx, `INSERT INTO dm_messages(conversation_id,sender_id,receiver_id,message_type,content)
		VALUES($1,$2,$3,$4,$5) RETURNING `+dmMessageColumns, input.ConversationID, input.SenderID, input.ReceiverID, input.Type, input.Content))
	if err != nil {
		return dm.Message{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE dm_conversations SET last_message=$2,last_message_time=$3,hidden_by_fan=false,hidden_by_anchor=false WHERE id=$1`,
		input.ConversationID, message.Content, message.CreatedAt)
	return message, err
}

func scanDMMessage(row pgx.Row) (dm.Message, error) {
	var msg dm.Message
	err := row.Scan(&msg.ID, &msg.ConversationID, &msg.SenderID, &msg.ReceiverID, &msg.Type, &msg.Content, &msg.Status, &msg.CreatedAt)
	msg.CreatedAt = msg.CreatedAt.UTC()
	return msg, err
}

func dmError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return dm.ErrNotFound
	}
	return err
}
func (s *Store) LatestDMFromSenders(ctx context.Context, receiver int64) (map[int64]time.Time, error) {
	rows, err := s.pool.Query(ctx, "SELECT sender_id,max(create_time) FROM dm_messages WHERE receiver_id=$1 GROUP BY sender_id", receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]time.Time)
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		result[id] = at.UTC()
	}
	return result, rows.Err()
}
