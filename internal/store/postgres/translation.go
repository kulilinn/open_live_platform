package postgres

import (
	"context"
	"slices"
	"time"
)

const selectRoomLanguages = `SELECT ARRAY(
    SELECT trans_lang AS lang FROM room_members WHERE room_id=rooms.id AND user_id<>rooms.owner_id AND trans_lang<>''
    UNION SELECT anchor_trans_lang WHERE anchor_trans_lang<>''
    ORDER BY lang
), live_id, owner_id FROM rooms WHERE id=$1 AND online`

func (r *roomChange) translationLanguages() ([]string, error) {
	var languages []string
	var liveID, ownerID int64
	err := r.tx.QueryRow(r.ctx, selectRoomLanguages, r.id).Scan(&languages, &liveID, &ownerID)
	return languages, err
}

func (r *roomChange) recordTranslationChange(before []string) ([]string, error) {
	var languages []string
	var liveID, ownerID int64
	err := r.tx.QueryRow(r.ctx, selectRoomLanguages, r.id).Scan(&languages, &liveID, &ownerID)
	if err == nil && !slices.Equal(before, languages) {
		r.translationChange = roomSignal{RoomID: r.id, Kind: "translate_change", LiveID: liveID, CreatedAt: time.Now().UTC()}
	}
	return languages, err
}

func (s *Store) ChangeViewerTranslation(ctx context.Context, userID int64, language string) error {
	var roomID *int64
	if err := s.pool.QueryRow(ctx, "SELECT current_viewer_room FROM users WHERE id=$1", userID).Scan(&roomID); err != nil {
		return err
	}
	if roomID == nil {
		return nil
	}
	return s.change(ctx, *roomID, func(r *roomChange) error {
		var active bool
		if err := r.tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM room_members AS m JOIN rooms ON rooms.id=m.room_id JOIN users ON users.id=m.user_id
			WHERE m.room_id=$1 AND m.user_id=$2 AND online AND users.current_viewer_room=m.room_id
		)`, *roomID, userID).Scan(&active); err != nil || !active {
			return err
		}
		before, err := r.translationLanguages()
		if err != nil {
			return err
		}
		if _, err = r.tx.Exec(ctx, "UPDATE room_members SET trans_lang=$3 WHERE room_id=$1 AND user_id=$2", *roomID, userID, language); err != nil {
			return err
		}
		_, err = r.recordTranslationChange(before)
		return err
	})
}
