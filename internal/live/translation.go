package live

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"
)

func TranslationPreferenceChanged(before, after bool, oldLanguage, language string) bool {
	return before != after || (after && oldLanguage != language)
}

func ActiveTranslationLanguage(enabled bool, language string) string {
	if enabled {
		return cmp.Or(language, "en")
	}
	return ""
}

func TranslationChangeEvent(ownerID int64, changedAt time.Time, languages []string) Event {
	return Event{Type: "translate_change", UserID: ownerID, CreatedAt: changedAt,
		Payload: cmp.Or(string(roomItemIDs(languages)), "[]")}
}

func (r *roomState) translationLanguages() []string {
	languages := make(map[string]struct{})
	if r.anchorLanguage != "" {
		languages[r.anchorLanguage] = struct{}{}
	}
	for _, language := range r.viewerLanguages {
		if language != "" {
			languages[language] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(languages))
}

func (r *roomState) publishTranslationChange(before []string) []string {
	languages := r.translationLanguages()
	if !slices.Equal(before, languages) {
		r.publish(TranslationChangeEvent(r.OwnerID, time.Now().UTC(), languages))
	}
	return languages
}

func (s *Store) ChangeViewerTranslation(ctx context.Context, userID int64, language string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	room := s.rooms[s.viewerRooms[userID]]
	if room != nil && room.Online && room.hasMember(userID) {
		before := room.translationLanguages()
		room.viewerLanguages[userID] = language
		room.publishTranslationChange(before)
	}
	return nil
}
