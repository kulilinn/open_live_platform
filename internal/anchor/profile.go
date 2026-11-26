package anchor

import (
	"context"
	"errors"
	"sync"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/subscription"
)

const (
	Pending = iota
	Approved
	Rejected
)

var (
	ErrNotFound = errors.New("data not found ")
	ErrPending  = errors.New("anchor approval is required")
	ErrNameUsed = errors.New("search name already in use")
)

type Record struct {
	ID         int64
	State      int
	Reason     string
	Channel    string
	KYCStatus  int
	StartedAt  *time.Time
	CreatedAt  time.Time
	FamilyName string
	Address    *string
	Intro      string
	Welcome    string
	Country    string
	Label      string
	FreeSub    int
	Social
}

type Application struct {
	FamilyName string  `json:"family_name"`
	Twitter    *string `json:"twitter"`
	IdentityForm
}

type IdentityForm struct {
	Name           *string `json:"name"`
	FirstName      *string `json:"first_name"`
	SecondName     *string `json:"second_name"`
	JapaneseFirst  *string `json:"ja_first_name"`
	JapaneseSecond *string `json:"ja_second_name"`
	Birth          *string `json:"birth"`
	Gender         *int8   `json:"gender"`
	Country        string  `json:"country"`
	Area           *string `json:"area"`
	Code           *string `json:"code"`
	Address        *string `json:"address"`
	DocumentType   *int8   `json:"id_card_type"`
	Document       *string `json:"id_card"`
	Phone          *string `json:"phone_number"`
	Front          *string `json:"id_card_front_pic"`
	Back           *string `json:"id_card_back_pic"`
	Holding        *string `json:"id_card_hold_pic"`
	Extra          *string `json:"extra"`
}

// Verified applicants can update this subset of their submitted form.
type IdentityUpdate struct {
	Name, Birth, Country, Area, Code, Address, Extra *string
	Gender                                           *int8
	IDCard, PhoneNumber                              *string
}

type Social struct {
	Twitter   string `json:"twitter_link"`
	Instagram string `json:"instagram_link"`
	StripChat string `json:"strip_chat_link"`
	TikTok    string `json:"tiktok_link"`
	YouTube   string `json:"yutube_link"`
}

type ProfileEdit struct {
	Address     *string `json:"address"`
	SearchName  string  `json:"search_name"`
	DisplayName string  `json:"display_name"`
	Intro       string  `json:"intro"`
	Country     string  `json:"country"`
	Banner      string  `json:"banner"`
	Image       string  `json:"img"`
	Twitter     string  `json:"twitter"`
	Instagram   string  `json:"instagram_link"`
	TikTok      string  `json:"tiktok_link"`
	YouTube     string  `json:"yutube_link"`
	Translate   *bool   `json:"trans"`
	Language    *string `json:"trans_lang"`
}

func (r *Record) Apply(input Application) {
	if input.FamilyName != "" {
		r.FamilyName = input.FamilyName
	}
	if input.Country != "" {
		r.Country = input.Country
	}
	if input.Address != nil {
		r.Address = input.Address
	}
	if input.Twitter != nil {
		r.Twitter = *input.Twitter
	}
}

func (r *Record) Edit(change ProfileEdit) {
	r.Address, r.Intro, r.Country = change.Address, change.Intro, change.Country
	r.Twitter, r.Instagram, r.TikTok, r.YouTube = change.Twitter, change.Instagram, change.TikTok, change.YouTube
}

type Store interface {
	Anchor(context.Context, int64) (Record, error)
	ApplyAnchor(context.Context, auth.User, Application) error
	EditAnchor(context.Context, int64, ProfileEdit) error
	EditAnchorSocial(context.Context, int64, Social) error
	AnchorApplicant(context.Context, int64) (string, error)
	SaveAnchorApplicant(context.Context, int64, string) error
	ConfirmAnchorKYC(context.Context, int64, string, IdentityUpdate) (bool, error)
}

func FromIdentity(u auth.User) (Record, error) {
	if u.Profile == nil || !u.Profile.IsAnchor {
		return Record{}, ErrNotFound
	}
	return Record{ID: u.ID, State: Approved, FreeSub: 1}, nil
}

type Memory struct {
	mu         sync.Mutex
	users      *auth.Store
	plans      subscription.Store
	records    map[int64]Record
	applicants map[int64]string
}

func NewMemory(users *auth.Store, plans subscription.Store) *Memory {
	return &Memory{users: users, plans: plans, records: make(map[int64]Record), applicants: make(map[int64]string)}
}

func (m *Memory) Anchor(ctx context.Context, id int64) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.anchor(ctx, id)
}

func (m *Memory) anchor(ctx context.Context, id int64) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if entry, ok := m.records[id]; ok {
		return entry, nil
	}
	user, _, err := m.users.Find(ctx, id)
	if err != nil {
		return Record{}, err
	}
	return FromIdentity(user)
}

func (m *Memory) ApplyAnchor(ctx context.Context, user auth.User, apply Application) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.anchor(ctx, user.ID)
	if errors.Is(err, ErrNotFound) {
		record = Record{ID: user.ID, State: Pending, FreeSub: 1, CreatedAt: time.Now().UTC()}
		if err := m.plans.EnsureFreePlan(ctx, user.ID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	record.Apply(apply)
	m.records[user.ID] = record
	return nil
}

func (m *Memory) EditAnchor(ctx context.Context, id int64, change ProfileEdit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.anchor(ctx, id)
	if err != nil || record.State != Approved {
		return ErrPending
	}
	if err := m.users.UpdateAnchorProfile(ctx, id, change.DisplayName, change.SearchName, change.Image, change.Banner,
		change.Translate, change.Language); err != nil {
		if errors.Is(err, auth.ErrSearchNameExists) {
			return ErrNameUsed
		}
		return err
	}
	record.Edit(change)
	m.records[id] = record
	return nil
}

func (m *Memory) EditAnchorSocial(ctx context.Context, id int64, social Social) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.anchor(ctx, id)
	if err != nil || record.State != Approved {
		return ErrPending
	}
	record.Social = social
	m.records[id] = record
	return nil
}

func (m *Memory) AnchorApplicant(ctx context.Context, uid int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applicants[uid], ctx.Err()
}

func (m *Memory) SaveAnchorApplicant(ctx context.Context, id int64, applicant string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous := m.applicants[id]; previous != "" && previous != applicant {
		return ErrKYC
	}
	m.applicants[id] = applicant
	return nil
}

func (m *Memory) ConfirmAnchorKYC(ctx context.Context, id int64, applicant string, _ IdentityUpdate) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	record, ok := m.records[id]
	if !ok || m.applicants[id] != applicant {
		return false, nil
	}
	record.KYCStatus = 1
	m.records[id] = record
	return true, nil
}
