package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

var ErrGiftTemplateExists = errors.New("gift template already exists")

type GiftSheetBatch struct {
	Gifts     []gift.Gift
	Boxes     []gift.Box
	Shortcuts []gift.QuickGift
	Limits    []gift.Limit
}

type GiftTemplateStore interface {
	GiftTemplates(ctx context.Context) (gift.Interactions, error)
	SaveGiftPromotion(ctx context.Context, promotion gift.Promotion, action string) error
	SaveGiftLimit(ctx context.Context, limit gift.Limit, action string) error
	SaveGiftBox(ctx context.Context, box gift.Box, action string) error
	SaveGiftShortcut(ctx context.Context, entry gift.QuickGift, action string) error
}

type giftTemplatePage struct {
	PageRequest
	ID           *string `json:"id"`
	GiftID       *string `json:"giftId"`
	PromotionID  *string `json:"promotionId"`
	Name         *string `json:"name"`
	DiscountType *int8   `json:"discountTy"`
	Status       *int8   `json:"status"`
	Start        string  `json:"startTime"`
	End          string  `json:"endTime"`
}

func templateMatches(value *string, id int64) bool {
	return value == nil || *value == strconv.FormatInt(id, 10)
}

type giftActivityEdit struct {
	ID          int64  `json:"id,string"`
	GiftID      int64  `json:"giftId,string"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Start       string `json:"startTime"`
	End         string `json:"endTime"`
}

type giftLimitEdit struct {
	ID           int64  `json:"id,string"`
	GiftID       int64  `json:"productId,string"`
	PromotionID  int64  `json:"promotionId,string"`
	PerUser      int64  `json:"maxPurchasePerUser"`
	Platform     int64  `json:"maxPurchasePlatform"`
	Price        int64  `json:"discountPrice"`
	Income       *int64 `json:"discountIncome"`
	DiscountType int8   `json:"discountType"`
	Discount     int32  `json:"discountValue"`
	Listing      int8   `json:"listingStatus"`
	Status       int8   `json:"status,omitempty"`
	Start        string `json:"startTime"`
	End          string `json:"endTime"`
}

const legacyTemplateTime = "2006-01-02 15:04:05 UTC"

func (b backoffice) registerGiftTemplates(mux *http.ServeMux, catalog *gift.Catalog) {
	store, ok := b.store.(GiftTemplateStore)
	if !ok {
		return
	}
	register := func(path string, handle backofficeHandler) {
		mux.HandleFunc("POST /api/operation/gifts/"+path, b.authorized(false, true, handle))
	}
	registerGiftCollections(register, store)
	for _, action := range []string{"page", "list", "detail"} {
		register("activity/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var query giftTemplatePage
			if !httpx.Decode(w, r, &query) {
				return
			}
			var start, end time.Time
			if query.Start != "" && query.End != "" {
				start, end = parseTemplateTime(query.Start), parseTemplateTime(query.End)
				if start.IsZero() || end.IsZero() || end.Before(start) {
					backofficeError(w, http.StatusBadRequest, "400", "invalid promotion time range")
					return
				}
			}
			content, err := store.GiftTemplates(r.Context())
			if err != nil {
				backofficeReply(w, nil, err)
				return
			}
			var data = make([]any, 0)
			for _, p := range content.Promotions {
				switch action {
				case "detail":
					if query.ID != nil && templateMatches(query.ID, p.ID) {
						backofficeReply(w, giftActivityEdit{p.ID, p.GiftID, p.Name, p.Description,
							p.Starts.UTC().Format(legacyTemplateTime), p.Ends.UTC().Format(legacyTemplateTime)}, nil)
						return
					}
				case "list":
					data = append(data, map[string]string{"id": strconv.FormatInt(p.ID, 10), "name": p.Name})
				case "page":
					if templateMatches(query.ID, p.ID) && (query.Name == nil || strings.HasPrefix(p.Name, *query.Name)) &&
						(start.IsZero() || !p.Starts.Before(start) && !p.Ends.After(end)) {
						data = append(data, p)
					}
				}
			}
			switch action {
			case "detail":
				backofficeReply(w, nil, ErrBackofficeNotFound)
			case "list":
				backofficeReply(w, data, nil)
			case "page":
				giftTemplateWindow(w, query.PageRequest, data)
			}
		})
	}
	for _, action := range []string{"add", "edit", "delete"} {
		register("activity/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var input giftActivityEdit
			if !httpx.Decode(w, r, &input) {
				return
			}
			start, end := parseTemplateTime(input.Start), parseTemplateTime(input.End)
			if input.ID <= 0 || action != "delete" && (input.GiftID <= 0 || strings.TrimSpace(input.Name) == "" || start.IsZero() || !start.Before(end)) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid promotion")
				return
			}
			if action != "delete" && !backofficeGiftExists(w, r, catalog, input.GiftID) {
				return
			}
			err := store.SaveGiftPromotion(r.Context(), gift.Promotion{ID: input.ID, GiftID: input.GiftID, Name: input.Name,
				Description: input.Description, Starts: start, Ends: end}, action)
			giftTemplateReply(w, err)
		})
	}
	for _, action := range []string{"page", "detail"} {
		register("product/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
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
			for _, l := range content.Limits {
				if !templateMatches(query.ID, l.ID) || !templateMatches(query.GiftID, l.GiftID) || !templateMatches(query.PromotionID, l.PromotionID) ||
					(query.Status != nil && *query.Status != l.Listing) || (query.DiscountType != nil && *query.DiscountType != l.DiscountType) {
					continue
				}
				startFormat := time.RFC3339Nano
				if action == "detail" {
					startFormat = legacyTemplateTime
				}
				item := giftLimitEdit{ID: l.ID, GiftID: l.GiftID, PromotionID: l.PromotionID, PerUser: l.PerUser, Platform: l.Platform,
					Price: l.Price, Income: l.Income, DiscountType: l.DiscountType, Discount: l.Discount, Listing: l.Listing,
					Start: l.Starts.UTC().Format(startFormat), End: l.Ends.UTC().Format(startFormat)}
				if action == "detail" && query.ID != nil {
					backofficeReply(w, item, nil)
					return
				}
				data = append(data, item)
			}
			if action == "detail" {
				backofficeReply(w, nil, ErrBackofficeNotFound)
				return
			}
			giftTemplateWindow(w, query.PageRequest, data)
		})
	}
	for _, action := range []string{"add", "edit", "delete", "update_status"} {
		register("product/"+action, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var input giftLimitEdit
			if !httpx.Decode(w, r, &input) {
				return
			}
			start, end := parseTemplateTime(input.Start), parseTemplateTime(input.End)
			if input.ID <= 0 || (action == "update_status" && input.Status != 0 && input.Status != 1) ||
				((action == "add" || action == "edit") && (input.GiftID <= 0 || input.PromotionID < 0 || input.PerUser < 0 || input.Platform < 0 ||
					input.Price < 0 || input.Income == nil || *input.Income < 0 || *input.Income > input.Price ||
					input.Listing < 0 || input.Listing > 1 || start.IsZero() || !start.Before(end))) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid gift purchase limit")
				return
			}
			if (action == "add" || action == "edit") && !backofficeGiftExists(w, r, catalog, input.GiftID) {
				return
			}
			if action == "update_status" {
				input.Listing = input.Status
			}
			err := store.SaveGiftLimit(r.Context(), gift.Limit{ID: input.ID, GiftID: input.GiftID, PromotionID: input.PromotionID,
				Starts: start, Ends: end, Price: input.Price, Income: input.Income, DiscountType: input.DiscountType,
				Discount: input.Discount, PerUser: input.PerUser, Platform: input.Platform, Listing: input.Listing}, action)
			giftTemplateReply(w, err)
		})
	}
}

func parseTemplateTime(value string) time.Time {
	if parsed, err := time.Parse(legacyTemplateTime, value); err == nil {
		return parsed
	}
	return parsePurchaseTime(value)
}

func giftTemplateWindow(w http.ResponseWriter, page PageRequest, data []any) {
	_, size, offset := page.Window()
	total := int64(len(data))
	offset = min(total, offset)
	backofficePage(w, data[offset:min(total, offset+size)], page, total, nil)
}

func giftTemplateReply(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGiftTemplateExists):
		backofficeError(w, http.StatusOK, "4032", err.Error())
	case errors.Is(err, gift.ErrLimit):
		backofficeError(w, http.StatusConflict, "409", err.Error())
	default:
		backofficeReply(w, nil, err)
	}
}

func backofficeGiftExists(w http.ResponseWriter, r *http.Request, catalog *gift.Catalog, id int64) bool {
	_, exists, err := catalog.Lookup(r.Context(), id)
	if err == nil && !exists {
		err = ErrBackofficeNotFound
	}
	if err != nil {
		backofficeReply(w, nil, err)
	}
	return err == nil
}
