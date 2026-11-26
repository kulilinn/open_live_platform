package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/transport/httpx"
)

const (
	OperationArea   = "admin"
	MCNArea         = "mcn"
	OperationTenant = "operation-admin"
	FinanceTenant   = "finance-admin"
	AccessTTL       = 2 * time.Hour
	RefreshTTL      = 7 * 24 * time.Hour
)

var ErrBackofficeNotFound = errors.New("backoffice record not found")

type Route struct {
	Path, Name, Icon string
	Routes           []Route `json:"routes"`
}

func (r Route) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Path   string  `json:"path"`
		Name   string  `json:"name"`
		Icon   string  `json:"icon"`
		Routes []Route `json:"routes"`
	}{r.Path, r.Name, r.Icon, nonNil(r.Routes)})
}

type Principal struct {
	ID              int64    `json:"id,string"`
	Area            string   `json:"area"`
	Tenant          string   `json:"tenant"`
	Username        string   `json:"username"`
	Nickname        string   `json:"nickName"`
	Email           string   `json:"email"`
	PasswordHash    string   `json:"passwordHash"`
	Admin           bool     `json:"admin"`
	AgencyID        int64    `json:"mcnId,string"`
	Manager         bool     `json:"manager"`
	Disabled        bool     `json:"disabled"`
	Roles           []string `json:"roles"`
	Permissions     []string `json:"permissions"`
	Routes          []Route  `json:"routes"`
	DatabaseVersion string   `json:"-"`
	AccountType     int      `json:"-"`
	password        auth.LegacyPassword
	fingerprint     [sha256.Size]byte
}

type Principals []Principal

func ReadPrincipals(file string) (Principals, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var principals Principals
	if err := json.Unmarshal(content, &principals); err != nil || len(principals) == 0 {
		return nil, errors.New("backoffice credential file must contain principals")
	}
	ids, logins := make(map[string]bool), make(map[string]bool)
	for i := range principals {
		p := &principals[i]
		var valid bool
		p.password, valid = auth.ParseLegacyPassword(p.PasswordHash)
		if !valid || p.ID <= 0 || (p.Area != OperationArea && p.Area != MCNArea) ||
			(p.Area == OperationArea && (p.Username == "" || p.Tenant == "")) || (p.Area == MCNArea && (p.Email == "" || p.AgencyID <= 0)) {
			return nil, fmt.Errorf("invalid backoffice principal at entry %d", i+1)
		}
		key := p.Area + ":" + strconv.FormatInt(p.ID, 10)
		login := p.Area + ":" + strings.ToLower(p.Name())
		if p.Area == OperationArea {
			login += ":" + p.Tenant
		}
		if ids[key] || logins[login] {
			return nil, fmt.Errorf("duplicate backoffice principal at entry %d", i+1)
		}
		ids[key], logins[login] = true, true
		// Changing credentials or tenant assignment invalidates existing sessions.
		p.fingerprint = sha256.Sum256([]byte(p.PasswordHash + "\x00" + p.Scope() + "\x00" + p.Name()))
		p.PasswordHash = ""
	}
	return principals, nil
}

func (p Principal) Name() string {
	if p.Area == MCNArea {
		return p.Email
	}
	return p.Username
}

func (p Principal) Scope() string {
	if p.Area == MCNArea {
		return strconv.FormatInt(p.AgencyID, 10)
	}
	return p.Tenant
}

type BackofficeSession struct {
	ID                            int64
	Area                          string
	AgencyID                      int64
	Manager                       bool
	AccountType                   int
	DisplayName                   string
	Fingerprint                   [sha256.Size]byte
	DatabaseVersion               string
	AccessHash, RefreshHash       [sha256.Size]byte
	AccessExpires, RefreshExpires time.Time
}

type BackofficeStore interface {
	LoginBackoffice(ctx context.Context, session BackofficeSession) error
	FindBackoffice(ctx context.Context, area string, hash [sha256.Size]byte, refresh bool) (int64, [sha256.Size]byte, error)
	RefreshBackoffice(ctx context.Context, session BackofficeSession, oldRefresh [sha256.Size]byte) error
	LogoutBackoffice(ctx context.Context, area string, access [sha256.Size]byte) error
	AuditBackofficeAccess(ctx context.Context, principal Principal, requestPath string) error
	BackofficeUsers(ctx context.Context, query UserQuery) ([]UserRow, int64, error)
	BackofficeRooms(ctx context.Context, query RoomQuery) ([]RoomRow, int64, error)
	BackofficeViolations(ctx context.Context, userID int64, page PageRequest) ([]Violation, int64, error)
	BackofficeLogs(ctx context.Context, principal Principal, page PageRequest) ([]Log, int64, error)
	ApplyBackofficeAction(ctx context.Context, principal Principal, operation Action) error
	BackofficeStreamers(ctx context.Context, principal Principal, query StreamerQuery) ([]StreamerRow, int64, error)
	BackofficeGiftPurchases(ctx context.Context, query PurchaseQuery) ([]PurchaseRow, int64, error)
}

type backoffice struct {
	store      BackofficeStore
	area       string
	principals Principals
	options    BackofficeOptions
}

type tokens struct {
	Access    string `json:"accessToken"`
	Refresh   string `json:"refreshToken"`
	ExpiresIn int64  `json:"expiresIn"`
	Type      string `json:"tokenType"`
}

func newBackofficeSession(p Principal) (BackofficeSession, tokens) {
	t := tokens{Access: "bo-" + rand.Text() + rand.Text(), Refresh: "br-" + rand.Text() + rand.Text(), ExpiresIn: int64(AccessTTL.Seconds()), Type: "Bearer"}
	now := time.Now().UTC()
	displayName := p.Nickname
	if displayName == "" {
		displayName = p.Name()
	}
	accountType := 0
	if p.AccountType != 0 {
		accountType = p.AccountType
	} else if p.Admin {
		accountType = 1
	} else if p.Manager {
		accountType = 2
	}
	return BackofficeSession{ID: p.ID, Area: p.Area, AgencyID: p.AgencyID, Manager: p.Manager,
		AccountType: accountType, DisplayName: displayName, Fingerprint: p.fingerprint, DatabaseVersion: p.DatabaseVersion,
		AccessHash: sha256.Sum256([]byte(t.Access)), RefreshHash: sha256.Sum256([]byte(t.Refresh)),
		AccessExpires: now.Add(AccessTTL), RefreshExpires: now.Add(RefreshTTL)}, t
}

func BackofficeHandler(area string, principals Principals, store BackofficeStore, catalog *gift.Catalog, options ...BackofficeOptions) (http.Handler, error) {
	if !slices.ContainsFunc(principals, func(p Principal) bool { return p.Area == area && !p.Disabled }) {
		return nil, fmt.Errorf("no active %s principals in the configured credential file", area)
	}
	b := backoffice{store: store, area: area, principals: principals}
	if system, ok := store.(SystemStore); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := system.ReserveSystemBootstrap(ctx, principals)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	if len(options) > 0 {
		b.options = options[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthchecker", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200", "message": "Loverose Server"})
	})
	login, logout := "POST /api/auth/accountlogin", "POST /api/auth/logout"
	if area == MCNArea {
		login, logout = "POST /api/user/account_login", "POST /api/user/logout"
	}
	mux.HandleFunc(login, b.login)
	b.registerSystem(mux)
	mux.HandleFunc(logout, b.authorized(false, false, func(w http.ResponseWriter, r *http.Request, _ Principal, key [sha256.Size]byte) {
		backofficeReply(w, nil, b.store.LogoutBackoffice(r.Context(), b.area, key))
	}))
	mux.HandleFunc("POST /api/auth/refreshtoken", b.authorized(true, false, func(w http.ResponseWriter, r *http.Request, p Principal, old [sha256.Size]byte) {
		session, response := newBackofficeSession(p)
		if err := b.store.RefreshBackoffice(r.Context(), session, old); errors.Is(err, ErrBackofficeNotFound) {
			backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
		} else {
			backofficeReply(w, response, err)
		}
	}))
	if area == MCNArea {
		if err := validateMCNInviteURL(b.options.MCNJoinURL); err != nil {
			return nil, err
		}
		b.registerMCN(mux)
		b.registerMCNOnboarding(mux)
		b.registerMCNReports(mux)
		b.registerMCNIncome(mux)
	} else {
		if b.options.ConfigDiagnostics != nil {
			mux.HandleFunc("GET /testconfig", b.options.ConfigDiagnostics)
		}
		b.registerOperations(mux)
		b.registerUserHistory(mux)
		b.registerCatalogAndFinance(mux, catalog)
		b.registerGiftMutations(mux, catalog)
		b.registerAnchorReviews(mux)
		b.registerAnchorRecords(mux)
		b.registerPostReviews(mux)
		b.registerEditorial(mux)
		b.registerNFTManagement(mux)
		b.registerPlatformSettings(mux)
		b.registerFinancePayments(mux)
		b.registerPlatformStats(mux)
		b.registerWordLists(mux)
		b.registerUploads(mux, catalog)
	}
	b.registerMCNDirectory(mux)
	b.registerMCNFinanceIncome(mux)
	return managementMux{mux}, nil
}

type managementMux struct{ *http.ServeMux }

func (m managementMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var limit int64 = 64 * 1024
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		limit = 2 * 1024 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	m.ServeMux.ServeHTTP(w, r)
}

func (b backoffice) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Email, Password string }
	if !httpx.Decode(w, r, &body) {
		return
	}
	name, tenant := body.Username, r.Header.Get("X-Loverose-Admin")
	if b.area == MCNArea {
		name = body.Email
	}
	for _, p := range b.principals {
		if p.Area != b.area || !strings.EqualFold(p.Name(), name) || (b.area == OperationArea && p.Tenant != tenant) {
			continue
		}
		b.loginPrincipal(w, r, p, body.Password)
		return
	}
	if system, ok := b.store.(SystemStore); ok && name != "" && (b.area != OperationArea || tenant != "") && len(body.Password) <= 4096 {
		p, err := system.FindSystemPrincipal(r.Context(), b.area, 0, tenant, name)
		if err == nil {
			b.loginPrincipal(w, r, p, body.Password)
			return
		}
		if !errors.Is(err, ErrBackofficeNotFound) {
			backofficeReply(w, nil, err)
			return
		}
	}
	backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
}

func (b backoffice) loginPrincipal(w http.ResponseWriter, r *http.Request, p Principal, password string) {
	if p.Disabled || password == "" || len(password) > 4096 || !p.password.Matches(password) {
		backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
		return
	}
	session, response := newBackofficeSession(p)
	err := b.store.LoginBackoffice(r.Context(), session)
	if errors.Is(err, ErrBackofficeNotFound) {
		backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
		return
	}
	backofficeReply(w, response, err)
}

func (b backoffice) currentPrincipal(ctx context.Context, id int64) (Principal, error) {
	for _, p := range b.principals {
		if p.Area == b.area && p.ID == id {
			return p, nil
		}
	}
	if system, ok := b.store.(SystemStore); ok {
		return system.FindSystemPrincipal(ctx, b.area, id, "", "")
	}
	return Principal{}, ErrBackofficeNotFound
}

type backofficeHandler func(http.ResponseWriter, *http.Request, Principal, [sha256.Size]byte)

func (b backoffice) authorized(refresh, permission bool, next backofficeHandler) http.HandlerFunc {
	tenant := ""
	if permission && b.area == OperationArea {
		tenant = OperationTenant
	}
	return b.authorizedTenant(refresh, permission, tenant, next)
}

func (b backoffice) authorizedTenant(refresh, permission bool, tenant string, next backofficeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || len(bearer) > 200 || bearer == "" {
			backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
			return
		}
		key := sha256.Sum256([]byte(bearer))
		id, fingerprint, err := b.store.FindBackoffice(r.Context(), b.area, key, refresh)
		if errors.Is(err, ErrBackofficeNotFound) {
			backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
			return
		}
		if err != nil {
			backofficeReply(w, nil, err)
			return
		}
		p, err := b.currentPrincipal(r.Context(), id)
		if errors.Is(err, ErrBackofficeNotFound) || (err == nil && (p.Disabled || p.fingerprint != fingerprint)) {
			backofficeError(w, http.StatusUnauthorized, "401", "Unauthorized request")
			return
		}
		if err != nil {
			backofficeReply(w, nil, err)
			return
		}
		if permission && ((tenant != "" && p.Tenant != tenant) || !p.Can(systemPermission(r.URL.Path))) {
			backofficeError(w, http.StatusForbidden, "403", "Operation not allowed")
			return
		}
		if permission {
			if err := b.store.AuditBackofficeAccess(r.Context(), p, r.URL.Path); err != nil {
				backofficeReply(w, nil, err)
				return
			}
		}
		next(w, r, p, key)
	}
}

func backofficeError(w http.ResponseWriter, code int, status, message string) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, code, map[string]string{"status": status, "message": message})
}

func backofficeReply(w http.ResponseWriter, data any, err error) {
	if errors.Is(err, ErrBackofficeNotFound) {
		backofficeError(w, http.StatusOK, "4005", "data not found ")
		return
	}
	if err != nil {
		backofficeError(w, http.StatusInternalServerError, "500", "Internal error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	result := map[string]any{"status": "200"}
	if data != nil {
		result["data"] = data
	}
	httpx.JSON(w, http.StatusOK, result)
}

func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
