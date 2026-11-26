package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/live"
	"open_live_platform/internal/posts"
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
	var input auth.UserProfilePatch
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.IdentityChanged() || input.Trans != nil || input.Language != nil {
		ctx := r.Context()
		user, found, err := s.accounts.Find(ctx, userID)
		if err != nil || !found {
			httpx.Error(w, http.StatusInternalServerError, "could not read user settings")
			return
		}
		if input.SearchName != nil {
			if len(*input.SearchName) < 3 || len(*input.SearchName) > 15 {
				anchorStatus(w, http.StatusBadRequest, "4072", "invalid search name length")
				return
			}
			if !anchorSearchName.MatchString(*input.SearchName) {
				anchorStatus(w, http.StatusOK, "4033", "invalid search name")
				return
			}
		}
		for _, value := range []*string{input.DisplayName, input.Photo, input.Banner} {
			if value != nil && strings.ContainsRune(*value, 0) {
				httpx.Error(w, http.StatusBadRequest, "invalid profile content")
				return
			}
		}
		profile := auth.PublicProfile(user)
		ctx, ok := submitStaticReviews(w, ctx, s.staticReviewer, userID,
			staticContent{typeID: posts.ReviewAvatar, object: userID, kind: "user_avatar", before: profile.Photo, after: optionalStaticValue(input.Photo), refusal: "4055", image: true},
			staticContent{typeID: posts.ReviewDisplayName, object: userID, kind: "user_display_name", before: user.Account, after: optionalStaticValue(input.DisplayName), refusal: "4058"},
			staticContent{typeID: posts.ReviewSearchName, object: userID, kind: "user_search_name", before: profile.SearchName, after: optionalStaticValue(input.SearchName), refusal: "4059"})
		if !ok {
			return
		}
		if err := s.accounts.UpdateUserProfile(ctx, userID, input); errors.Is(err, auth.ErrSearchNameExists) {
			anchorStatus(w, http.StatusOK, "4032", err.Error())
			return
		} else if err != nil {
			httpx.Error(w, http.StatusInternalServerError, "could not update user settings")
			return
		}
		enabled, language := user.TranslationEnabled, user.TranslationLanguage
		if input.Trans != nil {
			enabled = *input.Trans
		}
		if input.Language != nil {
			language = *input.Language
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
		"teacherLabel": s.teacherLabel(r.Context(), p.GroupRoom),
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

func (s server) teacherLabel(ctx context.Context, groupRoom string) string {
	id, err := strconv.ParseInt(groupRoom, 10, 64)
	if err != nil || id <= 0 || s.rooms == nil {
		return ""
	}
	room, err := s.rooms.Info(ctx, id)
	if err != nil {
		return ""
	}
	return room.TeacherLabel
}
