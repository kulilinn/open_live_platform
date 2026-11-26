package httpapi

import (
	"errors"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"open_live_platform/internal/anchor"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/live"
	"open_live_platform/internal/posts"
	"open_live_platform/internal/subscription"
	"open_live_platform/internal/transport/httpx"
)

type anchorAPI struct {
	server
	store    anchor.Store
	reviewer posts.Reviewer
	plans    subscription.Store
	kyc      *anchor.KYC
}

func RegisterAnchorProfiles(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, store anchor.Store,
	plans subscription.Store, collectibles *collectible.Service, reviewer posts.Reviewer, kyc *anchor.KYC) {
	s := anchorAPI{server: server{accounts: accounts, rooms: rooms, collectibles: collectibles}, store: store,
		reviewer: reviewer, plans: plans, kyc: kyc}
	for path, handler := range map[string]userHandler{
		"POST /anchor/api/become_anchor":      s.apply,
		"POST /anchor/api/is_anchor":          s.status,
		"POST /anchor/api/get_info":           s.getProfile,
		"POST /anchor/api/set_info":           s.updateProfile,
		"POST /anchor/api/set_socialize_info": s.updateSocial,
		"POST /anchor/api/kyc_get_token":      s.kycToken,
		"POST /anchor/api/kyc_set_info":       s.kycConfirm,
	} {
		mux.HandleFunc(path, s.authenticated(handler))
	}
}

func (s anchorAPI) apply(w http.ResponseWriter, r *http.Request, actor int64) {
	var input anchor.Application
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !httpx.Decode(w, r, &input) {
		return
	}
	err := s.store.ApplyAnchor(r.Context(), auth.User{ID: actor}, input)
	anchorMutation(w, err)
}

func (s anchorAPI) status(w http.ResponseWriter, r *http.Request, actor int64) {
	record, err := s.store.Anchor(r.Context(), actor)
	if errors.Is(err, anchor.ErrNotFound) {
		anchorResult(w, map[string]bool{"is_anchor": false})
		return
	}
	if err != nil {
		anchorError(w, err)
		return
	}
	result := map[string]any{"is_anchor": record.State == anchor.Approved, "state": record.State, "reject_reason": ""}
	if record.State == anchor.Rejected {
		result["reject_reason"] = record.Reason
	} else {
		started := record.StartedAt
		if tasks, ok := s.store.(anchor.TaskStore); ok {
			first, err := tasks.StarTaskStarted(r.Context(), actor)
			if err != nil {
				anchorError(w, err)
				return
			}
			if first != nil && (started == nil || first.Before(*started)) {
				started = first
			}
		}
		result["task_time_expired"] = started != nil && time.Since(*started) >= anchor.TaskEligibility
	}
	anchorResult(w, result)
}

func (s anchorAPI) getProfile(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		ID *int64 `json:"anchor_id,string"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.ID == nil || *input.ID < 0 {
		httpx.Error(w, http.StatusBadRequest, "invalid anchor ID")
		return
	}
	id := *input.ID
	if id == 0 {
		id = actor
	}
	ctx := r.Context()
	record, err := s.store.Anchor(ctx, id)
	if err != nil || (id != actor && record.State != anchor.Approved) {
		if err == nil {
			err = anchor.ErrNotFound
		}
		anchorError(w, err)
		return
	}
	user, found, err := s.accounts.Find(ctx, id)
	if err != nil || !found {
		if err == nil {
			err = anchor.ErrNotFound
		}
		anchorError(w, err)
		return
	}
	profile := auth.PublicProfile(user)
	_, image, err := s.collectibles.UserProfile(ctx, id, profile.Photo, false)
	if err != nil {
		anchorError(w, err)
		return
	}
	plans, err := s.plans.Plans(ctx, id, true)
	if err != nil {
		anchorError(w, err)
		return
	}
	free := record.FreeSub
	for _, p := range plans {
		if p.Type == 2 && p.Deleted == 0 {
			free = p.Published
			break
		}
	}
	result := map[string]any{
		"family_name": "", "name": profile.Name, "address": nil, "twitter": record.Twitter,
		"country": record.Country, "img": image, "banner": profile.Banner, "intro": record.Intro, "welcome": record.Welcome,
		"search_name": profile.SearchName, "display_name": user.Account, "free_sub": free, "toy_info": profile.ToyInfo,
		"instagram_link": record.Instagram, "strip_chat_link": record.StripChat, "tiktok_link": record.TikTok,
		"yutube_link": record.YouTube, "level": nil, "exp": nil, "max_exp": nil,
		"trans": user.TranslationEnabled, "trans_lang": user.TranslationLanguage,
		"teacher_label": s.teacherLabel(ctx, profile.GroupRoom), "room": profile.GroupRoom,
	}
	if id == actor {
		result["family_name"], result["address"] = record.FamilyName, record.Address
	}
	anchorResult(w, result)
}

var anchorSearchName = regexp.MustCompile(`^[A-Za-z0-9_-]{3,15}$`)

func (s anchorAPI) updateProfile(w http.ResponseWriter, r *http.Request, actor int64) {
	record, err := s.store.Anchor(r.Context(), actor)
	if errors.Is(err, anchor.ErrNotFound) || (err == nil && record.State != anchor.Approved) {
		err = anchor.ErrPending
	}
	if err != nil {
		anchorError(w, err)
		return
	}
	var change anchor.ProfileEdit
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if !httpx.Decode(w, r, &change) {
		return
	}
	if length := utf8.RuneCountInString(change.DisplayName); length < 2 || length > 15 {
		anchorStatus(w, http.StatusBadRequest, "4070", "invalid display name length")
		return
	}
	if length := len(change.SearchName); length < 3 || length > 15 {
		anchorStatus(w, http.StatusBadRequest, "4072", "invalid search name length")
		return
	}
	if !anchorSearchName.MatchString(change.SearchName) {
		anchorStatus(w, http.StatusOK, "4033", "invalid search name")
		return
	}
	user, found, err := s.accounts.Find(r.Context(), actor)
	if err != nil || !found {
		if err == nil {
			err = anchor.ErrNotFound
		}
		anchorError(w, err)
		return
	}
	profile := auth.PublicProfile(user)
	checks := []staticContent{
		{typeID: posts.ReviewAvatar, kind: "anchor_avatar", before: profile.Photo, after: change.Image, refusal: "4055", image: true},
		{typeID: posts.ReviewDisplayName, kind: "anchor_display_name", before: user.Account, after: change.DisplayName, refusal: "4058"},
		{typeID: posts.ReviewSearchName, kind: "anchor_search_name", before: profile.SearchName, after: change.SearchName, refusal: "4059"},
		{typeID: posts.ReviewProfileBanner, kind: "anchor_banner", before: profile.Banner, after: change.Banner, refusal: "4066", image: true},
		{typeID: posts.ReviewIntro, kind: "anchor_intro", before: record.Intro, after: change.Intro, refusal: "4067"},
	}
	if s.reviewer == nil {
		for _, change := range checks {
			if change.after != "" && change.after != change.before {
				anchorStatus(w, http.StatusOK, "4069", "profile audit service unavailable")
				return
			}
		}
	}
	ctx, ok := submitStaticReviews(w, r.Context(), s.reviewer, actor, checks...)
	if !ok {
		return
	}
	if err := s.store.EditAnchor(ctx, actor, change); err != nil {
		anchorError(w, err)
		return
	}
	enabled, language := user.TranslationEnabled, user.TranslationLanguage
	if change.Translate != nil {
		enabled = *change.Translate
	}
	if change.Language != nil {
		language = *change.Language
	}
	if live.TranslationPreferenceChanged(user.TranslationEnabled, enabled, user.TranslationLanguage, language) {
		if err := s.rooms.ChangeViewerTranslation(r.Context(), actor, live.ActiveTranslationLanguage(enabled, language)); err != nil {
			anchorError(w, err)
			return
		}
	}
	anchorMutation(w, nil)
}

func (s anchorAPI) updateSocial(w http.ResponseWriter, r *http.Request, actor int64) {
	var social anchor.Social
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if httpx.Decode(w, r, &social) {
		anchorMutation(w, s.store.EditAnchorSocial(r.Context(), actor, social))
	}
}

func (s anchorAPI) kycToken(w http.ResponseWriter, r *http.Request, actor int64) {
	ctx := r.Context()
	record, err := s.store.Anchor(ctx, actor)
	if err != nil && !errors.Is(err, anchor.ErrNotFound) {
		anchorError(w, err)
		return
	}
	if record.State == anchor.Approved || s.kyc == nil {
		anchorError(w, anchor.ErrKYC)
		return
	}
	applicant, err := s.store.AnchorApplicant(ctx, actor)
	if err != nil {
		anchorError(w, err)
		return
	}
	if applicant == "" {
		applicant, err = s.kyc.Applicant(ctx, actor)
		if err == nil {
			err = s.store.SaveAnchorApplicant(ctx, actor, applicant)
		}
		if err != nil {
			anchorError(w, err)
			return
		}
	}
	token, err := s.kyc.Token(ctx, actor)
	if err != nil {
		anchorError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	collectibleData(w, token)
}

func (s anchorAPI) kycConfirm(w http.ResponseWriter, r *http.Request, actor int64) {
	var body anchor.IdentityUpdate
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if !httpx.Decode(w, r, &body) {
		return
	}
	ctx := r.Context()
	applicant, err := s.store.AnchorApplicant(ctx, actor)
	if err != nil {
		anchorError(w, err)
		return
	}
	if applicant == "" {
		collectibleData(w, false)
		return
	}
	// Only the provider can confirm verification of this applicant.
	verified, err := s.kyc.Verified(ctx, actor, applicant)
	if err != nil {
		anchorError(w, err)
		return
	}
	if verified {
		verified, err = s.store.ConfirmAnchorKYC(ctx, actor, applicant, body)
		if err != nil {
			anchorError(w, err)
			return
		}
	}
	collectibleData(w, verified)
}

func anchorResult(w http.ResponseWriter, data any) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "result": data})
}

func anchorMutation(w http.ResponseWriter, err error) {
	if err == nil {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	} else {
		anchorError(w, err)
	}
}

func anchorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, anchor.ErrNotFound):
		anchorStatus(w, http.StatusOK, "4005", err.Error())
	case errors.Is(err, anchor.ErrPending):
		anchorStatus(w, http.StatusUnauthorized, "401", err.Error())
	case errors.Is(err, anchor.ErrNameUsed):
		anchorStatus(w, http.StatusOK, "4032", err.Error())
	case errors.Is(err, anchor.ErrKYC):
		anchorStatus(w, http.StatusOK, "4048", err.Error())
	default:
		httpx.Error(w, http.StatusInternalServerError, "anchor operation failed")
	}
}

func anchorStatus(w http.ResponseWriter, code int, status, message string) {
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}
