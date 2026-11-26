package collectible

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

var ErrManagedNFTConflict = errors.New("NFT idempotency key conflict")
var ErrManagedNFTInvalid = errors.New("invalid NFT management request")

type ManagedCard struct {
	ID              uint32     `json:"id"`
	Name            *string    `json:"name"`
	English         string     `json:"cardEnglishName"`
	Chinese         string     `json:"cardChineseName"`
	Picture         string     `json:"cardPicName"`
	Status          int8       `json:"status"`
	OrderAccount    *string    `json:"orderAccount"`
	PreOrderAccount *string    `json:"preOrderAccount"`
	PreOrderName    *string    `json:"preOrderName"`
	Description     *string    `json:"description"`
	Created         *time.Time `json:"createTime"`
	Updated         *time.Time `json:"updateTime"`
	Sale            *time.Time `json:"saleTime"`
	Sold            *time.Time `json:"soldTime"`
	UserID          *int64     `json:"userId,string"`
	Account         *string    `json:"account"`
	Inventory       bool       `json:"-"`
}

type ManagedCardFilter struct {
	ID       *uint32 `json:"id"`
	English  *string `json:"cardEnName"`
	Name     *string `json:"name"`
	Statuses []int8  `json:"status"`
}

type ManagedCardEdit struct {
	ID              uint32     `json:"id"`
	Name            *string    `json:"name"`
	Status          int8       `json:"status"`
	PreOrderName    *string    `json:"accountName"`
	PreOrderAccount *string    `json:"accountAddress"`
	Description     *string    `json:"description"`
	Sale            *time.Time `json:"saleTime"`
}

type ManagedNFTRequest struct {
	Actor  int64
	Key    string
	Digest [32]byte
}

type ManagedNFTStore interface {
	ManagedNFTCards(ctx context.Context) ([]ManagedCard, error)
	HasManagedNFTRequest(ctx context.Context, req ManagedNFTRequest) (bool, error)
	EditManagedNFTCard(ctx context.Context, req ManagedNFTRequest, edit ManagedCardEdit, seed []Card) error
	SetManagedNFTSale(ctx context.Context, req ManagedNFTRequest, ids []uint32, sale time.Time, seed []Card) error
	BindManagedNFT(ctx context.Context, req ManagedNFTRequest, userID int64, binding Binding, slots map[int8]int32) error
	UnbindManagedNFT(ctx context.Context, req ManagedNFTRequest, userID int64) error
}

func (s *Service) ManagedCards(ctx context.Context, query ManagedCardFilter, offset, limit int64) ([]ManagedCard, int64, error) {
	store, ok := s.store.(ManagedNFTStore)
	if !ok {
		return nil, 0, ErrUnavailable
	}
	if len(query.Statuses) > 5 || slices.ContainsFunc(query.Statuses, func(v int8) bool { return v < 1 || v > 5 }) {
		return nil, 0, ErrManagedNFTInvalid
	}
	rows, err := store.ManagedNFTCards(ctx)
	if err != nil {
		return nil, 0, err
	}
	items := make(map[uint32]ManagedCard, len(s.data.Cards)+len(rows))
	for _, item := range s.data.Cards {
		card := ManagedCard{ID: item.ID, English: item.English, Chinese: item.Chinese, Picture: item.Picture, Status: item.Status, Inventory: true}
		if item.Name != "" {
			card.Name = &item.Name
		}
		items[item.ID] = card
	}
	for _, item := range rows {
		if item.Inventory {
			items[item.ID] = item
		} else if card, ok := items[item.ID]; ok {
			card.UserID, card.Account = item.UserID, item.Account
			items[item.ID] = card
		}
	}
	config := s.luban.Load()
	page := make([]ManagedCard, 0, len(items))
	for _, card := range items {
		if config != nil {
			if meta, ok := config.cards[card.ID]; ok {
				card.English, card.Chinese, card.Picture = meta.English, meta.Chinese, meta.Picture
			}
		}
		if query.ID != nil && card.ID != *query.ID || query.English != nil && !strings.EqualFold(card.English, *query.English) ||
			query.Name != nil && (card.Name == nil || !strings.EqualFold(*card.Name, *query.Name)) ||
			len(query.Statuses) > 0 && !slices.Contains(query.Statuses, card.Status) {
			continue
		}
		page = append(page, card)
	}
	slices.SortFunc(page, func(a, b ManagedCard) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	total := int64(len(page))
	if offset >= total {
		return []ManagedCard{}, total, nil
	}
	return page[offset:min(offset+limit, total)], total, nil
}

func (s *Service) ManageCardEdit(ctx context.Context, req ManagedNFTRequest, edit ManagedCardEdit) error {
	store, ok := s.store.(ManagedNFTStore)
	if !ok {
		return ErrUnavailable
	}
	if edit.Status < 1 || edit.Status > 5 || !validManagedText(edit.Name, 256) || !validManagedText(edit.Description, 4096) ||
		!validManagedText(edit.PreOrderName, 256) || !validManagedText(edit.PreOrderAccount, 256) {
		return ErrManagedNFTInvalid
	}
	return store.EditManagedNFTCard(ctx, req, edit, s.managedNFTSeeds([]uint32{edit.ID}))
}

func (s *Service) ManageSaleTime(ctx context.Context, req ManagedNFTRequest, ids []uint32, sale time.Time) error {
	store, ok := s.store.(ManagedNFTStore)
	if !ok {
		return ErrUnavailable
	}
	if len(ids) == 0 || len(ids) > 1000 || sale.IsZero() {
		return ErrManagedNFTInvalid
	}
	ids = append([]uint32(nil), ids...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return store.SetManagedNFTSale(ctx, req, ids, sale, s.managedNFTSeeds(ids))
}

func (s *Service) ManageBind(ctx context.Context, req ManagedNFTRequest, userID int64, tokenID uint32, account string) error {
	store, ok := s.store.(ManagedNFTStore)
	if !ok {
		return ErrUnavailable
	}
	if userID <= 0 {
		return ErrManagedNFTInvalid
	}
	if req.Key != "" {
		if completed, err := store.HasManagedNFTRequest(ctx, req); completed || err != nil {
			return err
		}
	}
	binding, err := s.VerifyOwnership(ctx, tokenID, account)
	if err != nil {
		return err
	}
	return store.BindManagedNFT(ctx, req, userID, binding, s.defaultNFTSlots())
}

func (s *Service) ManageUnbind(ctx context.Context, req ManagedNFTRequest, userID int64) error {
	store, ok := s.store.(ManagedNFTStore)
	if !ok {
		return ErrUnavailable
	}
	if userID <= 0 {
		return ErrManagedNFTInvalid
	}
	return store.UnbindManagedNFT(ctx, req, userID)
}

func (s *Service) managedNFTSeeds(ids []uint32) []Card {
	var seeds []Card
	for _, card := range s.data.Cards {
		if slices.Contains(ids, card.ID) {
			seeds = append(seeds, card)
		}
	}
	return seeds
}

func validManagedText(text *string, limit int) bool {
	return text == nil || (len(*text) <= limit && !strings.ContainsRune(*text, 0))
}

func (s *Service) defaultNFTSlots() map[int8]int32 {
	if adorns := s.availableAdorns(); len(adorns) > 0 && adorns[0].Unlock == 1 {
		return adorns[0].slots()
	}
	return nil
}
