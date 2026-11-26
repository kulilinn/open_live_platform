package admin

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

func registerGiftCollections(register func(string, backofficeHandler), store GiftTemplateStore) {
	for _, action := range []string{"page", "detail"} {
		register("gift_box/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var query giftTemplatePage
			if !httpx.Decode(w, r, &query) {
				return
			}
			content, err := store.GiftTemplates(r.Context())
			if err != nil {
				backofficeReply(w, nil, err)
				return
			}
			data := make([]any, 0)
			for _, box := range content.Boxes {
				if !templateMatches(query.ID, box.ID) || query.ID == nil && query.Name != nil && *query.Name != box.Name {
					continue
				}
				if action == "detail" && query.ID != nil {
					backofficeReply(w, box, nil)
					return
				}
				data = append(data, box)
			}
			if action == "detail" {
				backofficeReply(w, nil, ErrBackofficeNotFound)
				return
			}
			giftTemplateWindow(w, query.PageRequest, data)
		})
		register("gift_shortcut/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var query giftTemplatePage
			if !httpx.Decode(w, r, &query) {
				return
			}
			content, err := store.GiftTemplates(r.Context())
			if err != nil {
				backofficeReply(w, nil, err)
				return
			}
			data := make([]any, 0)
			for _, entry := range content.QuickGifts {
				if !templateMatches(query.ID, entry.ID) || !templateMatches(query.GiftID, entry.ID) {
					continue
				}
				var limit any
				if entry.LimitID != 0 {
					limit = strconv.FormatInt(entry.LimitID, 10)
				}
				if action == "detail" && query.ID != nil {
					backofficeReply(w, map[string]any{"giftId": strconv.FormatInt(entry.ID, 10), "limitId": limit}, nil)
					return
				}
				data = append(data, map[string]any{"giftId": strconv.FormatInt(entry.ID, 10), "limitIds": limit})
			}
			if action == "detail" {
				backofficeReply(w, nil, ErrBackofficeNotFound)
				return
			}
			giftTemplateWindow(w, query.PageRequest, data)
		})
	}
	for _, action := range []string{"add", "edit", "delete"} {
		register("gift_box/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var box gift.Box
			if !httpx.Decode(w, r, &box) {
				return
			}
			if box.ID <= 0 || action != "delete" && (strings.TrimSpace(box.Name) == "" || !giftBoxIDsValid(box.LimitIDs)) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid gift box")
				return
			}
			giftTemplateReply(w, store.SaveGiftBox(r.Context(), box, action))
		})
		register("gift_shortcut/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var request struct {
				ID     int64 `json:"id,string"`
				GiftID int64 `json:"giftId,string"`
				Limit  int64 `json:"limitId,string"`
			}
			if !httpx.Decode(w, r, &request) {
				return
			}
			id := request.GiftID
			if action == "delete" {
				id = request.ID
			}
			if id <= 0 || request.Limit < 0 {
				backofficeError(w, http.StatusBadRequest, "400", "invalid gift shortcut")
				return
			}
			giftTemplateReply(w, store.SaveGiftShortcut(r.Context(), gift.QuickGift{ID: id, LimitID: request.Limit}, action))
		})
	}
	register("gift_shortcut/empty", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		giftTemplateReply(w, store.SaveGiftShortcut(r.Context(), gift.QuickGift{}, "empty"))
	})
}

func giftBoxIDsValid(values string) bool {
	for _, value := range strings.Split(values, ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil || id <= 0 {
			return false
		}
	}
	return true
}
