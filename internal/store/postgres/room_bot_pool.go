package postgres

import (
	"context"

	"open_live_platform/internal/livebot"
)

type LegacyRoomBotPoolResult struct {
	Imported, Existing, Missing, Idle, InUse int
}

func (s *Store) ImportLegacyRoomBotPool(ctx context.Context, entries []livebot.LegacyPoolEntry) (LegacyRoomBotPoolResult, error) {
	var result LegacyRoomBotPoolResult
	byID := make(map[int64]string, len(entries))
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return LegacyRoomBotPoolResult{}, err
		}
		if _, seen := byID[entry.UserID]; !seen {
			ids = append(ids, entry.UserID)
		}
		if byID[entry.UserID] != "inuse" {
			byID[entry.UserID] = entry.State
		}
	}
	for _, state := range byID {
		if state == "inuse" {
			result.InUse++
		} else {
			result.Idle++
		}
	}
	var found int
	if err := s.pool.QueryRow(ctx, `WITH candidates AS (
		SELECT id FROM users WHERE id=ANY($1::bigint[]) ORDER BY id FOR KEY SHARE
	), imported AS (
		INSERT INTO room_bot_pool(user_id) SELECT id FROM candidates ORDER BY id ON CONFLICT DO NOTHING RETURNING user_id
	) SELECT (SELECT count(*) FROM candidates),(SELECT count(*) FROM imported)`, ids).Scan(&found, &result.Imported); err != nil {
		return LegacyRoomBotPoolResult{}, err
	}
	result.Existing = found - result.Imported
	result.Missing = len(ids) - found
	return result, nil
}
