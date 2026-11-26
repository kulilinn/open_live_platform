package collectible

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	ErrUnavailable = errors.New("NFT verification unavailable")
	ErrSignature   = errors.New("NFT signature error")
	ErrAlready     = errors.New("NFT already binding")
	ErrNotOwned    = errors.New("NFT not owned")
	ErrUnbound     = errors.New("NFT unbinding")
	ErrEquip       = errors.New("adorn equip error")
	ErrNotFound    = errors.New("data not found ")
)

type Config struct {
	RPC, Contract, CatalogFile string
	DefaultPhoto, BoundPhoto   string
	Treasury, SyncToken        string
}

func ConfigFromEnv() Config {
	return Config{RPC: os.Getenv("WEB3_HTTP"), Contract: os.Getenv("WEB3_CONTRACT_ADDRESS"),
		CatalogFile: os.Getenv("NFT_CATALOG_FILE"), DefaultPhoto: os.Getenv("USER_PHOTO_DEFAULT"),
		BoundPhoto: os.Getenv("USER_NFT_PHOTO_DEFAULT"), Treasury: os.Getenv("WEB3_ACCOUNT"),
		SyncToken: os.Getenv("NFT_SYNC_TOKEN")}
}

type Challenge struct {
	ID      int64     `json:"challengeId,string"`
	Message string    `json:"challenge"`
	UserID  int64     `json:"-"`
	Expires time.Time `json:"-"`
}

type Binding struct {
	TokenID uint32
	Address string
}

type State struct {
	Binding *Binding
	Slots   map[int8]int32
}

type Store interface {
	SaveNFTChallenge(context.Context, Challenge) error
	NFTChallenge(context.Context, int64, int64) (Challenge, error)
	NFTState(context.Context, int64) (State, error)
	BindNFT(context.Context, Challenge, Binding, map[int8]int32) error
	UnbindNFT(context.Context, int64, *Binding) error
	EquipNFT(context.Context, int64, Binding, map[int8]int32) error
	UnequipNFT(context.Context, int64, int8, int32) error
	NFTCards(context.Context) ([]Card, error)
	SaveNFTCards(context.Context, []Card, bool) error
}

type Privilege struct {
	ID          int32  `json:"id"`
	Group       int32  `json:"rightsGroup"`
	Icon        string `json:"rightsIcon"`
	Name        string `json:"rightsName"`
	Status      int32  `json:"rightsStatus"`
	Description string `json:"rightsDescription"`
	Redirect    string `json:"redirectPage"`
}

type Adorn struct {
	ID           int32  `json:"id"`
	Name         string `json:"outfitName"`
	Description  string `json:"outfitDescription"`
	Icon         string `json:"outfitIcon"`
	Unlock       int32  `json:"unlockCondition"`
	Entry        string `json:"entryEffect"`
	Floating     string `json:"entryFloatingScreen"`
	AvatarFrame  string `json:"exclusiveAvatarFrame"`
	ColoredName  string `json:"coloredName"`
	ChatBubble   string `json:"chatBubble"`
	Medal        string `json:"exclusiveMedal"`
	MedalTag     string `json:"exclusiveMedalTag"`
	EffectTime   int32  `json:"effectTime,omitempty"`
	FloatingTime int32  `json:"floatingScreenTime,omitempty"`
}

func (a Adorn) effects() [7]string {
	return [...]string{a.Entry, a.Floating, a.AvatarFrame, a.ColoredName, a.ChatBubble, a.Medal, a.MedalTag}
}

func (a Adorn) slots() map[int8]int32 {
	slots := make(map[int8]int32)
	for index, value := range a.effects() {
		if value != "" {
			slots[int8(index+1)] = a.ID
		}
	}
	return slots
}

type Equipped struct {
	ID       int32  `json:"adornId"`
	Type     int8   `json:"adornType"`
	Value    string `json:"value"`
	Duration int32  `json:"duration"`
	Expired  bool   `json:"isExpired"`
}

type View struct {
	TokenID  *uint32             `json:"tokenId"`
	Equipped map[string]Equipped `json:"equippedAdorn"`
}

type Card struct {
	ID      uint32 `json:"id"`
	Name    string `json:"name"`
	English string `json:"cardEnglishName"`
	Chinese string `json:"cardChineseName"`
	Picture string `json:"cardPicName"`
	Address string `json:"walletAddress"`
	Status  int8   `json:"status"`
}

type catalog struct {
	Privileges []Privilege `json:"privileges"`
	Adorns     []Adorn     `json:"adorns"`
	Cards      []Card      `json:"cards"`
	Inventory  []Card      `json:"inventory"`
}

type Service struct {
	luban     atomic.Pointer[LubanCatalog]
	store     Store
	chain     *chain
	data      catalog
	adorn     map[int32]Adorn
	photo     string
	nftPhoto  string
	treasury  string
	syncToken string
}

func New(config Config, store Store) (*Service, error) {
	rpc, err := newChain(config.RPC, config.Contract)
	if err != nil {
		return nil, err
	}
	s := &Service{store: store, chain: rpc, adorn: make(map[int32]Adorn),
		photo: config.DefaultPhoto, nftPhoto: config.BoundPhoto, syncToken: config.SyncToken}
	if config.Treasury != "" {
		var ok bool
		s.treasury, ok = wallet(config.Treasury)
		if !ok {
			return nil, errors.New("invalid WEB3_ACCOUNT")
		}
	}
	if config.SyncToken != "" && (s.treasury == "" || rpc == nil) {
		return nil, errors.New("NFT_SYNC_TOKEN requires WEB3_HTTP, WEB3_CONTRACT_ADDRESS and WEB3_ACCOUNT")
	}
	if config.CatalogFile == "" {
		return s, nil
	}
	data, err := os.ReadFile(config.CatalogFile)
	if err == nil {
		err = json.Unmarshal(data, &s.data)
	}
	if err != nil {
		return nil, fmt.Errorf("read NFT_CATALOG_FILE: %w", err)
	}
	for _, item := range s.data.Adorns {
		if _, exists := s.adorn[item.ID]; item.ID <= 0 || exists || item.EffectTime < 0 || item.FloatingTime < 0 {
			return nil, fmt.Errorf("invalid NFT catalog adorn %d", item.ID)
		}
		s.adorn[item.ID] = item
	}
	if err := validateCards(s.data.Cards, false); err != nil {
		return nil, err
	}
	if err := validateCards(s.data.Inventory, true); err != nil {
		return nil, err
	}
	return s, nil
}

func validateCards(cards []Card, inventory bool) error {
	ids := make(map[uint32]struct{}, len(cards))
	for i, card := range cards {
		if _, dup := ids[card.ID]; dup || (!inventory && (card.Status < 1 || card.Status > 5)) {
			return fmt.Errorf("invalid NFT catalog card %d", card.ID)
		}
		ids[card.ID] = struct{}{}
		if inventory {
			cards[i].Status, cards[i].Address = 5, ""
		} else if card.Address != "" {
			address, ok := wallet(card.Address)
			if !ok {
				return fmt.Errorf("invalid NFT catalog wallet for card %d", card.ID)
			}
			cards[i].Address = address
		}
	}
	return nil
}

func (s *Service) UserProfile(ctx context.Context, userID int64, photo string, includeEquipped bool) (View, string, error) {
	var view View
	var err error
	if includeEquipped || (photo == "" && s.nftPhoto != s.photo) {
		view, err = s.View(ctx, userID, false)
		if err != nil {
			return View{}, "", err
		}
	}
	if photo == "" {
		photo = s.photo
		if view.TokenID != nil {
			photo = s.nftPhoto
		}
	}
	return view, photo, nil
}

func (s *Service) Privileges() []Privilege {
	if cfg := s.luban.Load(); cfg != nil {
		return append([]Privilege{}, cfg.privileges...)
	}
	return append([]Privilege{}, s.data.Privileges...)
}

func (s *Service) Adorns(unlock *int32) []Adorn {
	items := []Adorn{}
	for _, item := range s.availableAdorns() {
		if unlock == nil || item.Unlock == *unlock {
			item.EffectTime, item.FloatingTime = 0, 0
			items = append(items, item)
		}
	}
	return items
}

func (s *Service) Cards(ctx context.Context, statuses []int8, page, size int) ([]Card, int, error) {
	inventory, err := s.cards(ctx)
	if err != nil {
		return nil, 0, err
	}
	all := make([]Card, 0, len(inventory))
	for _, card := range inventory {
		if len(statuses) == 0 || slices.Contains(statuses, card.Status) {
			all = append(all, card)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	items := []Card{}
	offset := min(len(all), (page-1)*size)
	for _, item := range all[offset:min(len(all), offset+size)] {
		if address := item.Address; len(address) > 15 {
			item.Address = address[:6] + "****" + address[len(address)-4:]
		}
		items = append(items, item)
	}
	return items, len(all), nil
}

func (s *Service) Challenge(ctx context.Context, userID int64) (Challenge, error) {
	var seed [48]byte
	if _, err := rand.Read(seed[8:]); err != nil {
		return Challenge{}, err
	}
	binary.LittleEndian.PutUint64(seed[:8], uint64(userID))
	digest := sha256.Sum256(seed[:40])
	id := int64(binary.LittleEndian.Uint64(seed[40:]) & math.MaxInt64)
	challenge := Challenge{ID: max(1, id), Message: hex.EncodeToString(digest[:]), UserID: userID,
		Expires: time.Now().Add(5 * time.Minute)}
	return challenge, s.store.SaveNFTChallenge(ctx, challenge)
}

func (s *Service) Bind(ctx context.Context, userID, challengeID int64, tokenID uint32, account, signature string) error {
	address, ok := wallet(account)
	if !ok {
		return ErrSignature
	}
	challenge, err := s.store.NFTChallenge(ctx, userID, challengeID)
	if err != nil {
		return err
	}
	if s.chain == nil {
		return ErrUnavailable
	}
	signer, err := s.chain.signer(ctx, challenge.Message, signature)
	if err != nil {
		return err
	}
	if signer != address {
		return ErrSignature
	}
	binding, err := s.VerifyOwnership(ctx, tokenID, address)
	if err != nil {
		return err
	}
	var slots map[int8]int32
	if adorns := s.availableAdorns(); len(adorns) > 0 && adorns[0].Unlock == 1 {
		slots = adorns[0].slots()
	}
	return s.store.BindNFT(ctx, challenge, binding, slots)
}

// VerifyOwnership confirms a wallet against ERC-721 ownerOf. Legacy imports
// separately rely on a prior wallet-signature proof recorded by the source DB.
func (s *Service) VerifyOwnership(ctx context.Context, tokenID uint32, account string) (Binding, error) {
	address, ok := wallet(account)
	if !ok {
		return Binding{}, ErrNotOwned
	}
	if s.chain == nil {
		return Binding{}, ErrUnavailable
	}
	owner, err := s.chain.owner(ctx, tokenID)
	if err != nil {
		return Binding{}, err
	}
	if owner != address {
		return Binding{}, ErrNotOwned
	}
	return Binding{TokenID: tokenID, Address: address}, nil
}

func (s *Service) Unbind(ctx context.Context, userID int64) error {
	return s.store.UnbindNFT(ctx, userID, nil)
}

func (s *Service) Equip(ctx context.Context, userID int64, adornID int32) error {
	adorn, found := s.adornLookup()[adornID]
	if !found {
		return ErrNotFound
	}
	if adorn.Unlock != 1 {
		return ErrEquip
	}
	state, err := s.verifiedState(ctx, userID, false)
	if err != nil {
		return err
	}
	if state.Binding == nil {
		return ErrEquip
	}
	return s.store.EquipNFT(ctx, userID, *state.Binding, adorn.slots())
}

func (s *Service) Unequip(ctx context.Context, userID int64, slot int8, id int32) error {
	return s.store.UnequipNFT(ctx, userID, slot, id)
}

func (s *Service) View(ctx context.Context, userID int64, verify bool) (View, error) {
	var state State
	var err error
	if verify {
		state, err = s.verifiedState(ctx, userID, true)
	} else {
		state, err = s.store.NFTState(ctx, userID)
	}
	view := View{Equipped: make(map[string]Equipped)}
	if err != nil || state.Binding == nil {
		return view, err
	}
	view.TokenID = &state.Binding.TokenID
	adorns := s.adornLookup()
	for slot, id := range state.Slots {
		a, found := adorns[id]
		if !found || slot < 1 || slot > 7 {
			continue
		}
		durations := [7]int32{a.EffectTime, a.FloatingTime}
		view.Equipped[strconv.Itoa(int(slot))] = Equipped{
			ID: id, Type: slot, Value: a.effects()[slot-1], Duration: durations[slot-1],
		}
	}
	return view, nil
}

func (s *Service) verifiedState(ctx context.Context, userID int64, allowStored bool) (State, error) {
	state, err := s.store.NFTState(ctx, userID)
	if err != nil || state.Binding == nil {
		return state, err
	}
	if s.chain == nil {
		if allowStored {
			return state, nil
		}
		return State{}, ErrUnavailable
	}
	owner, err := s.chain.owner(ctx, state.Binding.TokenID)
	if err != nil {
		if allowStored {
			return state, nil
		}
		return State{}, err
	}
	if owner == state.Binding.Address {
		return state, nil
	}
	if err := s.store.UnbindNFT(ctx, userID, state.Binding); err != nil {
		return State{}, err
	}
	// A concurrent bind may have changed the user's state during verification.
	return s.store.NFTState(ctx, userID)
}
