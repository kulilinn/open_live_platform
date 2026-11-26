package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

type giftPrice int64

var decimalPointAmount = regexp.MustCompile(`^[0-9]+(?:\.0+)?$`)

func (p *giftPrice) UnmarshalJSON(raw []byte) error {
	value := string(raw)
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
	}
	if len(value) > 40 || !decimalPointAmount.MatchString(value) {
		return errors.New("invalid gift point price")
	}
	amount, ok := new(big.Rat).SetString(value)
	if !ok || !amount.IsInt() || !amount.Num().IsInt64() {
		return errors.New("invalid gift point price")
	}
	*p = giftPrice(amount.Num().Int64())
	return nil
}

type giftAdminRequest struct {
	ID                int64      `json:"id,string"`
	Type              int8       `json:"ty"`
	Name              string     `json:"name"`
	Price             *giftPrice `json:"price"`
	Icon              string     `json:"icon"`
	Effects           *string    `json:"giftEffects"`
	Animation         *string    `json:"animation"`
	SpecialEffects    *string    `json:"animateEffect"`
	Vibration         int8       `json:"vibrationIntensity"`
	Toy               int8       `json:"checkToy"`
	Custom            int8       `json:"hasCustom"`
	Description       *string    `json:"recommendDesc"`
	ParameterOne      *string    `json:"param1"`
	ParameterTwo      *int32     `json:"param2"`
	ParameterThree    *int64     `json:"param3"`
	Income            int32      `json:"incomeStream"`
	Rule              int8       `json:"behaviorRule"`
	Duration          int32      `json:"behaviorTime"`
	Combo             int8       `json:"combo"`
	HardwareQueue     int8       `json:"hardwareQueueType"`
	Announcement      *string    `json:"announcement"`
	First             *string    `json:"announcement1"`
	Second            *string    `json:"announcement2"`
	Third             *string    `json:"announcement3"`
	EffectControlType int8       `json:"effectControlType"`
	Sort              *int32     `json:"giftSort"`
	BroadcastConfig   *int64     `json:"broadcastConfigId,string"`
}

func (r giftAdminRequest) gift() gift.Gift {
	return gift.Gift{ID: r.ID, Name: r.Name, Price: int64(*r.Price), Icon: r.Icon, GiftType: r.Type,
		SpecialEffects: r.SpecialEffects, Animation: r.Animation, NormalEffects: r.Effects, GiftSort: r.Sort,
		VibrationIntensity: r.Vibration, ToyInspection: r.Toy, IsCustom: &r.Custom, RecommendedDescription: r.Description,
		ParameterOne: r.ParameterOne, ParameterTwo: r.ParameterTwo, ParameterThree: r.ParameterThree, IncomeStream: r.Income,
		PerformanceRule: r.Rule, PerformanceTime: r.Duration, Combo: r.Combo, HardwareQueueType: r.HardwareQueue,
		Announcement: r.Announcement, AnnouncementOne: r.First, AnnouncementTwo: r.Second, AnnouncementThree: r.Third,
		EffectControlType: r.EffectControlType, BroadcastConfigID: r.BroadcastConfig}
}

func giftAdminDetail(g gift.Gift) map[string]any {
	var broadcast *string
	if g.BroadcastConfigID != nil {
		id := strconv.FormatInt(*g.BroadcastConfigID, 10)
		broadcast = &id
	}
	return map[string]any{
		"id": strconv.FormatInt(g.ID, 10), "ty": g.GiftType, "name": g.Name, "price": strconv.FormatInt(g.Price, 10), "icon": g.Icon,
		"giftEffects": g.NormalEffects, "animation": g.Animation, "animateEffect": g.SpecialEffects, "vibrationIntensity": g.VibrationIntensity,
		"checkToy": g.ToyInspection, "hasCustom": g.IsCustom, "recommendDesc": g.RecommendedDescription,
		"param1": g.ParameterOne, "param2": g.ParameterTwo, "param3": g.ParameterThree, "incomeStream": g.IncomeStream,
		"behaviorRule": g.PerformanceRule, "behaviorTime": g.PerformanceTime, "combo": g.Combo, "hardwareQueueType": g.HardwareQueueType,
		"announcement": g.Announcement, "announcement1": g.AnnouncementOne, "announcement2": g.AnnouncementTwo, "announcement3": g.AnnouncementThree,
		"effectControlType": g.EffectControlType, "giftSort": g.GiftSort, "broadcastConfigId": broadcast,
	}
}

type GiftCatalogEditor interface {
	EditGiftCatalog(ctx context.Context, actor Principal, path, action string, item gift.Gift, baseline bool) error
}

func (b backoffice) registerGiftMutations(mux *http.ServeMux, catalog *gift.Catalog) {
	const path = "/api/operation/gifts/gift/"
	mux.HandleFunc("POST "+path+"detail", b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query struct {
			ID int64 `json:"id,string"`
		}
		if !httpx.Decode(w, r, &query) {
			return
		}
		item, ok, err := catalog.Lookup(r.Context(), query.ID)
		if err == nil && !ok {
			err = ErrBackofficeNotFound
		}
		backofficeReply(w, giftAdminDetail(item), err)
	}))
	editor, ok := b.store.(GiftCatalogEditor)
	if !ok {
		return
	}
	for _, action := range []string{"add", "edit", "delete"} {
		mux.HandleFunc("POST "+path+action, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var input giftAdminRequest
			if !httpx.Decode(w, r, &input) {
				return
			}
			input.Name = strings.TrimSpace(input.Name)
			if input.ID <= 0 || (action != "delete" && (input.Price == nil || input.Name == "" || len(input.Name) > 256 ||
				input.Type < 0 || input.Type > 3 || input.EffectControlType < 0 || input.Income < 0 || int64(input.Income) > int64(*input.Price) || input.Duration < 0)) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid gift catalog entry")
				return
			}
			item := gift.Gift{ID: input.ID}
			if action != "delete" {
				item = input.gift()
			}
			err := editor.EditGiftCatalog(r.Context(), actor, r.URL.Path, action, item, catalog.Baseline(item.ID))
			if errors.Is(err, gift.ErrCatalogConflict) {
				backofficeError(w, http.StatusConflict, "409", err.Error())
				return
			}
			backofficeReply(w, nil, err)
		}))
	}
}
