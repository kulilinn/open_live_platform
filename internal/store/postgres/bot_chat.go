package postgres

import (
	"context"
	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/botchat"
)

const insertBotChat = `INSERT INTO bot_chat_messages(id,member_low,member_high,message) VALUES($1,$2,$3,$4)`

func (s *Store) SaveBotChat(ctx context.Context, m botchat.Message) error {
	_, err := s.pool.Exec(ctx, insertBotChat, m.ID, min(m.From, m.To), max(m.From, m.To), m)
	return err
}
func (s *Store) BotChatHistory(ctx context.Context, user, bot int64, after string) ([]botchat.Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT message FROM bot_chat_messages WHERE member_low=$1 AND member_high=$2 AND id>$3 ORDER BY id LIMIT $4`, min(user, bot), max(user, bot), after, botchat.PageSize+1)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[botchat.Message])
}
