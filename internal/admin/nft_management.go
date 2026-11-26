package admin

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"open_live_platform/internal/collectible"
	"open_live_platform/internal/transport/httpx"
)

var nftRequestKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (b backoffice) registerNFTManagement(mux *http.ServeMux) {
	service := b.options.Collectibles
	if service == nil {
		return
	}
	const prefix = "POST /api/operation/nft/creation_card/"
	mux.HandleFunc(prefix+"page", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query struct {
			PageRequest
			PageParams *PageRequest `json:"pageParams"`
			collectible.ManagedCardFilter
		}
		if !httpx.Decode(w, r, &query) {
			return
		}
		page := query.PageRequest
		if query.PageParams != nil {
			page = *query.PageParams
		}
		_, size, offset := page.Window()
		rows, total, err := service.ManagedCards(r.Context(), query.ManagedCardFilter, offset, size)
		if err != nil {
			backofficeNFTReply(w, err)
			return
		}
		backofficePage(w, rows, page, total, nil)
	}))
	mux.HandleFunc(prefix+"edit", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			ID *uint32 `json:"id"`
			collectible.ManagedCardEdit
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.ID == nil {
			backofficeNFTReply(w, collectible.ErrManagedNFTInvalid)
			return
		}
		edit := body.ManagedCardEdit
		edit.ID = *body.ID
		request, err := backofficeNFTRequest(r, p, edit)
		if err == nil {
			err = service.ManageCardEdit(r.Context(), request, edit)
		}
		backofficeNFTReply(w, err)
	}))
	mux.HandleFunc(prefix+"update_sale_time", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			IDs      []uint32 `json:"id"`
			SaleTime string   `json:"saleTime"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		sale, err := parseNFTSaleTime(body.SaleTime)
		if err != nil {
			backofficeNFTReply(w, err)
			return
		}
		slices.Sort(body.IDs)
		body.IDs = slices.Compact(body.IDs)
		body.SaleTime = sale.Format(time.RFC3339Nano)
		request, err := backofficeNFTRequest(r, p, body)
		if err == nil {
			err = service.ManageSaleTime(r.Context(), request, body.IDs, sale)
		}
		backofficeNFTReply(w, err)
	}))
	mux.HandleFunc(prefix+"binding", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			UserID  SystemID `json:"userId"`
			TokenID *uint32  `json:"tokenId"`
			Account string   `json:"account"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.TokenID == nil || body.UserID <= 0 {
			backofficeNFTReply(w, collectible.ErrManagedNFTInvalid)
			return
		}
		body.Account = "0x" + strings.TrimPrefix(strings.ToLower(body.Account), "0x")
		request, err := backofficeNFTRequest(r, p, body)
		if err == nil {
			err = service.ManageBind(r.Context(), request, body.UserID.Value(), *body.TokenID, body.Account)
		}
		backofficeNFTReply(w, err)
	}))
	mux.HandleFunc(prefix+"unbinding", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			UserID SystemID `json:"userId"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		request, err := backofficeNFTRequest(r, p, body)
		if err == nil {
			err = service.ManageUnbind(r.Context(), request, body.UserID.Value())
		}
		backofficeNFTReply(w, err)
	}))
}

func backofficeNFTRequest(r *http.Request, p Principal, body any) (collectible.ManagedNFTRequest, error) {
	key := r.Header.Get("Idempotency-Key")
	req := collectible.ManagedNFTRequest{Actor: p.ID, Key: key}
	if key == "" {
		return req, nil
	}
	if !nftRequestKey.MatchString(key) {
		return req, collectible.ErrManagedNFTInvalid
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return req, collectible.ErrManagedNFTInvalid
	}
	req.Digest = sha256.Sum256(append([]byte(r.URL.Path+"\x00"), encoded...))
	return req, nil
}

func parseNFTSaleTime(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, collectible.ErrManagedNFTInvalid
}

func backofficeNFTReply(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, collectible.ErrNotOwned):
		backofficeError(w, http.StatusOK, "4089", err.Error())
	case errors.Is(err, collectible.ErrAlready):
		backofficeError(w, http.StatusOK, "4088", err.Error())
	case errors.Is(err, collectible.ErrNotFound):
		backofficeReply(w, nil, ErrBackofficeNotFound)
	case errors.Is(err, collectible.ErrManagedNFTInvalid):
		backofficeError(w, http.StatusBadRequest, "400", err.Error())
	case errors.Is(err, collectible.ErrManagedNFTConflict):
		backofficeError(w, http.StatusConflict, "409", err.Error())
	case errors.Is(err, collectible.ErrUnavailable):
		backofficeError(w, http.StatusServiceUnavailable, "503", err.Error())
	default:
		backofficeReply(w, nil, err)
	}
}
