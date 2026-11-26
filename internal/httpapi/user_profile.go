package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

func (s server) setLanguage(w http.ResponseWriter, r *http.Request, userID int64) {
	language := r.Header.Get("x-loverose-lang")
	if language == "" {
		httpx.Error(w, http.StatusBadRequest, "x-loverose-lang is required")
		return
	}
	if err := s.accounts.SetLanguage(r.Context(), userID, language); err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not update language")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func (s server) setUserInfo(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		TranslationLanguage *string `json:"transLang"`
		TranslationEnabled  *bool   `json:"trans"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.TranslationLanguage != nil || input.TranslationEnabled != nil {
		user, found, err := s.accounts.Find(r.Context(), userID)
		if err != nil || !found {
			httpx.Error(w, http.StatusInternalServerError, "could not read translation settings")
			return
		}
		if err := s.accounts.SetTranslationSettings(r.Context(), userID, input.TranslationEnabled, input.TranslationLanguage); err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not update translation language")
			return
		}
		enabled, language := user.TranslationEnabled, user.TranslationLanguage
		if input.TranslationEnabled != nil {
			enabled = *input.TranslationEnabled
		}
		if input.TranslationLanguage != nil {
			language = *input.TranslationLanguage
		}
		if live.TranslationPreferenceChanged(user.TranslationEnabled, enabled, user.TranslationLanguage, language) {
			if err := s.rooms.ChangeViewerTranslation(r.Context(), userID, live.ActiveTranslationLanguage(enabled, language)); err != nil {
				httpx.Error(w, http.StatusInternalServerError, "could not update room translation language")
				return
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func (s server) checkUserStatus(w http.ResponseWriter, r *http.Request, userID int64) {
	user, found, err := s.accounts.Find(r.Context(), userID)
	if err != nil || !found {
		httpx.Error(w, http.StatusInternalServerError, "could not read account status")
		return
	}
	m := user.Moderation.Status()
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": "200",
		"data": map[string]any{
			"ty": m.Type, "allow": !m.ActiveAt(time.Now()), "limit_total": m.Total, "duration": m.Until, "reason": m.Reasons(),
		},
	})
}

func (s server) userApps(w http.ResponseWriter, _ *http.Request, _ int64) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": []any{}})
}

func (s server) userProfile(w http.ResponseWriter, r *http.Request, currentID int64) {
	targetID := currentID
	if value := r.PathValue("user_id"); value != "" && value != "me" {
		var err error
		targetID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || targetID < 1 {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
			return
		}
	}
	user, ok, err := s.accounts.Find(r.Context(), targetID)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "could not read user")
		return
	}
	if !ok {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "4005", "message": "data not found "})
		return
	}
	p := user.Profile
	if p == nil {
		p = &auth.Profile{Name: user.Account, SearchName: user.Account, ToyInfo: "{}", UserType: "normal"}
	}
	isMe := r.PathValue("user_id") == "me"
	view := collectible.View{Equipped: map[string]collectible.Equipped{}}
	photo := p.Photo
	if s.collectibles != nil {
		view, photo, err = s.collectibles.UserProfile(r.Context(), targetID, photo, isMe)
		if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not read user adornments")
			return
		}
	}
	m := user.Moderation.Status()
	data := map[string]any{
		"id": strconv.FormatInt(user.ID, 10), "name": p.Name,
		"gender": strconv.Itoa(p.Gender), "photo": photo, "online": 0,
		"popularity": p.Popularity, "isAnchor": p.IsAnchor,
		"toyInfo": p.ToyInfo, "anchorStatus": 0,
		"displayName": user.Account, "searchName": p.SearchName,
		"groupRoom": p.GroupRoom, "level": 0, "exp": 0, "maxExp": 0,
		"trans": user.TranslationEnabled, "transLang": user.TranslationLanguage, "limitTy": m.Type, "limitAllow": !m.ActiveAt(time.Now()),
		"limitTotal": m.Total, "limitTime": m.Until, "limitReason": m.Reasons(),
		"teacherLabel": "",
	}
	if isMe {
		data["verified"], data["banner"], data["nsfw"], data["userType"] = p.Verified, p.Banner, p.NSFW, p.UserType
		data["email"], data["language"], data["search_name"] = user.Email, user.Language, p.SearchName
		data["pt"], data["fansTotal"], data["token_id"] = 0, 0, view.TokenID
		data["xkey"], data["rkey"], data["promotion"] = "", "", ""
		data["equipped_adorn"] = view.Equipped
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}
