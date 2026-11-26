package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/botchat"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if sourceDSN == "" || targetDSN == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN and DATABASE_URL")
	}
	config, err := mysql.ParseDSN(sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	source := sql.OpenDB(connector)
	defer source.Close()
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("read MySQL snapshot: %w", err)
	}
	defer snapshot.Rollback()
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	counts, err := target.ImportLegacyInteractions(ctx,
		queryRows(ctx, snapshot, conversationsQuery, scanConversation),
		queryRows(ctx, snapshot, messagesQuery, scanMessage),
		queryRows(ctx, snapshot, noticesQuery, scanNotice),
		queryRows(ctx, snapshot, "SELECT id,`from`,`to`,message,translate,language,tts,image,video,time FROM chat_message ORDER BY id", scanBotMessage))
	if err != nil {
		return err
	}
	fmt.Printf("conversations=%d messages=%d notices=%d bot_messages=%d existing=%d skipped=%d\n", counts.Conversations, counts.Messages, counts.Notices, counts.BotMessages,
		counts.Existing, counts.Skipped)
	return nil
}

const conversationsQuery = `SELECT c.id,c.conversation_type,c.members,c.last_message,c.last_message_time,c.can_message,c.hide_data,
	c.metadata,c.del_flag,c.create_time,r.expires FROM dm_conversations c
	LEFT JOIN (SELECT uid,anchor_id,MAX(expire_at) expires FROM subscription_user_sub
		WHERE cancel_at IS NULL AND expire_at>UTC_TIMESTAMP() GROUP BY uid,anchor_id) r
	ON r.uid=CAST(JSON_UNQUOTE(JSON_EXTRACT(c.metadata,'$.fansInfo.uid')) AS SIGNED)
	AND r.anchor_id=CAST(JSON_UNQUOTE(JSON_EXTRACT(c.metadata,'$.anchorInfo.uid')) AS SIGNED)
	ORDER BY c.id`

const messagesQuery = `SELECT id,conversation_id,sender_id,COALESCE(receiver_id,0),COALESCE(group_id,0),message_type,content,status,del_flag,create_time
	FROM dm_messages ORDER BY id`

const noticesQuery = `SELECT id,notice_target,notice_type,receive_id,sender_avatar,content,biz_id,status,create_time,del_flag
	FROM notices ORDER BY id`

func queryRows[T any](ctx context.Context, snapshot *sql.Tx, query string, read func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := snapshot.QueryContext(ctx, query)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			value, err := read(rows)
			if !yield(value, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

func scanConversation(rows *sql.Rows) (postgres.LegacyDMConversation, error) {
	var c postgres.LegacyDMConversation
	var members, hidden, metadata, last []byte
	var deleted, blocked int
	var expires sql.NullTime
	err := rows.Scan(&c.ID, &c.Type, &members, &last, &c.LastActivity, &blocked, &hidden, &metadata, &deleted, &c.CreatedAt, &expires)
	if err != nil {
		return c, err
	}
	c.Deleted = deleted != 0
	if c.Deleted || c.Type != 1 {
		return c, nil
	}
	c.CanMessage = blocked == 0
	if expires.Valid {
		c.EntitlementUntil = &expires.Time
	}
	if err := json.Unmarshal(members, &c.Members); err != nil {
		return c, fmt.Errorf("conversation %d has invalid member IDs", c.ID)
	}
	if len(hidden) > 0 && json.Unmarshal(hidden, &c.Hidden) != nil {
		return c, fmt.Errorf("conversation %d has invalid hidden members", c.ID)
	}
	if err := json.Unmarshal(metadata, &c.Metadata); err != nil {
		return c, fmt.Errorf("conversation %d has invalid metadata", c.ID)
	}
	if err := json.Unmarshal(last, &c.LastMessage); err != nil {
		return c, fmt.Errorf("conversation %d has an unsupported last message", c.ID)
	}
	return c, nil
}

func scanBotMessage(rows *sql.Rows) (botchat.Message, error) {
	var m botchat.Message
	var created time.Time
	var audio, images, videos []byte
	if err := rows.Scan(&m.ID, &m.From, &m.To, &m.Message, &m.Translate, &m.Language, &audio, &images, &videos, &created); err != nil {
		return m, err
	}
	m.Time = strconv.FormatInt(created.Unix(), 10)
	if utf8.Valid(audio) {
		m.TTS = string(audio)
	}
	media := func(raw []byte) []string {
		var v []string
		if json.Unmarshal(raw, &v) != nil || v == nil {
			return []string{}
		}
		return v
	}
	m.Image, m.Video = media(images), media(videos)
	return m, nil
}

func scanMessage(rows *sql.Rows) (postgres.LegacyDMMessage, error) {
	var message postgres.LegacyDMMessage
	var content []byte
	var groupID int64
	var deleted int
	err := rows.Scan(&message.ID, &message.ConversationID, &message.SenderID, &message.ReceiverID, &groupID, &message.Type, &content,
		&message.Status, &deleted, &message.CreatedAt)
	if err != nil {
		return message, err
	}
	message.Deleted, message.Group = deleted != 0, groupID != 0
	if !message.Deleted && !message.Group && message.ReceiverID > 0 {
		if err := json.Unmarshal(content, &message.Content); err != nil {
			return message, fmt.Errorf("direct message %d has unsupported content", message.ID)
		}
	}
	return message, nil
}

func scanNotice(rows *sql.Rows) (postgres.LegacyNotice, error) {
	var n postgres.LegacyNotice
	var deleted int
	err := rows.Scan(&n.ID, &n.Target, &n.Type, &n.UserID, &n.Avatar, &n.Content, &n.ObjectID, &n.Status, &n.CreatedAt, &deleted)
	n.Deleted = deleted != 0
	return n, err
}
