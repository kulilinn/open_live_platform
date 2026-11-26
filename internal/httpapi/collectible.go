package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"open_live_platform/internal/collectible"
	"open_live_platform/internal/transport/httpx"
)

func (s server) registerCollectibles(mux *http.ServeMux) {
	if s.collectibles == nil {
		return
	}
	for path, handler := range map[string]userHandler{
		"GET /api/nft/creation_card/get_privilege_list": s.nftPrivileges,
		"GET /api/nft/creation_card/get_challenge":      s.nftChallenge,
		"GET /api/nft/creation_card/get_binding":        s.nftBinding,
		"POST /api/nft/creation_card/binding":           s.bindNFT,
		"POST /api/nft/creation_card/unbinding":         s.unbindNFT,
		"GET /api/adorn/get_list":                       s.adornList,
		"POST /api/adorn/equip":                         s.equipAdorn,
		"POST /api/adorn/unequip_by_id":                 s.unequipAdorn,
		"POST /api/adorn/unequip_by_type":               s.unequipAdorn,
		"GET /api/adorn/get_user_equipped":              s.equippedAdorn,
	} {
		mux.HandleFunc(path, s.authenticated(handler))
	}
	mux.HandleFunc("POST /api/nft/creation_card/page", s.nftPage)
	mux.HandleFunc("POST /api/nft/creation_card/init_data_20250523", s.syncNFTCards(true))
	mux.HandleFunc("POST /api/nft/creation_card/schedule_refresh", s.syncNFTCards(false))
}

func (s server) nftPrivileges(w http.ResponseWriter, _ *http.Request, _ int64) {
	collectibleData(w, s.collectibles.Privileges())
}

func (s server) nftChallenge(w http.ResponseWriter, r *http.Request, userID int64) {
	challenge, err := s.collectibles.Challenge(r.Context(), userID)
	if err != nil {
		collectibleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	collectibleData(w, challenge)
}

func (s server) nftBinding(w http.ResponseWriter, r *http.Request, userID int64) {
	view, err := s.collectibles.View(r.Context(), userID, true)
	if err != nil {
		collectibleError(w, err)
		return
	}
	collectibleData(w, view)
}

func (s server) bindNFT(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		Wallet      string  `json:"account"`
		Signature   string  `json:"signature"`
		ChallengeID int64   `json:"challengeId,string"`
		TokenID     *uint32 `json:"tokenId"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.ChallengeID <= 0 || input.TokenID == nil {
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid NFT binding")
		return
	}
	collectibleMutation(w, s.collectibles.Bind(r.Context(), userID, input.ChallengeID, *input.TokenID, input.Wallet, input.Signature))
}

func (s server) unbindNFT(w http.ResponseWriter, r *http.Request, userID int64) {
	collectibleMutation(w, s.collectibles.Unbind(r.Context(), userID))
}

func (s server) nftPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	input := struct {
		Statuses *[]int8        `json:"status"`
		Page     subscriptionID `json:"page"`
		PageSize subscriptionID `json:"pageSize"`
	}{Page: 1, PageSize: 20}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.Statuses == nil || input.Page < 1 || input.Page > 1<<31-1 || input.PageSize < 1 {
		httpx.Error(w, http.StatusUnprocessableEntity, "invalid NFT page")
		return
	}
	size := int(min(100, input.PageSize))
	items, total, err := s.collectibles.Cards(r.Context(), *input.Statuses, int(input.Page), size)
	if err != nil {
		collectibleError(w, err)
		return
	}
	collectibleData(w, map[string]any{"list": items, "current": int(input.Page), "pageSize": size, "total": total})
}

func (s server) syncNFTCards(initial bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.collectibles.AuthorizedSync(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpx.Error(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		collectibleMutation(w, s.collectibles.SyncCards(r.Context(), initial))
	}
}

func (s server) adornList(w http.ResponseWriter, r *http.Request, _ int64) {
	var unlock *int32
	if raw := r.URL.Query().Get("adornType"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid adorn type")
			return
		}
		filter := int32(value)
		unlock = &filter
	}
	collectibleData(w, s.collectibles.Adorns(unlock))
}

func (s server) equippedAdorn(w http.ResponseWriter, r *http.Request, userID int64) {
	view, err := s.collectibles.View(r.Context(), userID, false)
	if err != nil {
		collectibleError(w, err)
		return
	}
	collectibleData(w, view.Equipped)
}

func (s server) equipAdorn(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		ID int32 `json:"adornId"`
	}
	if httpx.Decode(w, r, &input) {
		collectibleMutation(w, s.collectibles.Equip(r.Context(), userID, input.ID))
	}
}

func (s server) unequipAdorn(w http.ResponseWriter, r *http.Request, userID int64) {
	var input struct {
		ID   *int32 `json:"adornId"`
		Type *int8  `json:"adornType"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if r.URL.Path == "/api/adorn/unequip_by_id" && input.ID != nil {
		collectibleMutation(w, s.collectibles.Unequip(r.Context(), userID, 0, *input.ID))
		return
	}
	if r.URL.Path == "/api/adorn/unequip_by_type" && input.Type != nil {
		if *input.Type == 0 {
			collectibleMutation(w, nil)
			return
		}
		collectibleMutation(w, s.collectibles.Unequip(r.Context(), userID, *input.Type, 0))
		return
	}
	httpx.Error(w, http.StatusUnprocessableEntity, "adorn ID or type is required")
}

func collectibleData(w http.ResponseWriter, data any) {
	httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": data})
}

func collectibleMutation(w http.ResponseWriter, err error) {
	if err != nil {
		collectibleError(w, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
}

func collectibleError(w http.ResponseWriter, err error) {
	var status string
	switch {
	case errors.Is(err, collectible.ErrNotFound):
		status = "4005"
	case errors.Is(err, collectible.ErrSignature):
		status = "4087"
	case errors.Is(err, collectible.ErrAlready):
		status = "4088"
	case errors.Is(err, collectible.ErrNotOwned):
		status = "4089"
	case errors.Is(err, collectible.ErrUnbound):
		status = "4090"
	case errors.Is(err, collectible.ErrEquip):
		status = "4091"
	case errors.Is(err, collectible.ErrUnavailable):
		httpx.Error(w, http.StatusServiceUnavailable, err.Error())
		return
	default:
		httpx.Error(w, http.StatusInternalServerError, "collectible operation failed")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": status, "message": err.Error()})
}
