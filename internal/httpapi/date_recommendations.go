package httpapi

import (
	"cmp"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/transport/httpx"
)

func (s server) dateRecommendations(w http.ResponseWriter, r *http.Request, _ int64) {
	ctx := r.Context()
	data := make([]map[string]any, 0)
	if s.dates == nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
		return
	}
	ids, err := s.dates.Service.Store.RecommendedDateHosts(ctx)
	if err != nil {
		dateResponse(w, err, nil)
		return
	}
	now := time.Now()
	for _, id := range ids {
		user, ok, err := s.accounts.Find(ctx, id)
		if err != nil {
			dateResponse(w, err, nil)
			return
		}
		if !ok || user.Moderation.Banned() {
			continue
		}
		host, session, err := s.dates.Service.Store.DateHost(ctx, id)
		if err != nil {
			dateResponse(w, err, nil)
			return
		}
		status, online := 0, 0
		switch {
		case session.Active(now):
			status, online = 2, 3
		case host.Pending.Active(now):
			status, online = 2, 2
		case host.Listening(now):
			status, online = 1, 2
		}
		profile := auth.PublicProfile(user)
		moderation := user.Moderation.Status()
		data = append(data, map[string]any{
			"id": strconv.FormatInt(user.ID, 10), "name": profile.Name, "displayName": user.Account,
			"searchName": profile.SearchName, "photo": profile.Photo, "gender": strconv.Itoa(profile.Gender),
			"isAnchor": true, "anchorStatus": status, "online": online, "popularity": profile.Popularity,
			"groupRoom": profile.GroupRoom, "teacherLabel": "", "toyInfo": cmp.Or(profile.ToyInfo, "{}"),
			"level": 0, "exp": 0, "maxExp": 0, "trans": user.TranslationEnabled, "transLang": user.TranslationLanguage,
			"limitTy": moderation.Type, "limitAllow": !moderation.ActiveAt(now), "limitTotal": moderation.Total,
			"limitTime": moderation.Until, "limitReason": moderation.Reasons(),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}
