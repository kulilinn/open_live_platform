package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"open_live_platform/internal/auth"
)

var (
	ErrSystemForbidden = errors.New("backoffice operation not allowed")
	ErrSystemInvalid   = errors.New("invalid backoffice system request")
	ErrSystemConflict  = errors.New("backoffice system record already exists or is in use")
)

type SystemStore interface {
	ReserveSystemBootstrap(context.Context, Principals) error
	FindSystemPrincipal(context.Context, string, int64, string, string) (Principal, error)
	ListSystemAccounts(context.Context, Principal, SystemFilter) ([]SystemAccount, int64, error)
	ListSystemRoles(context.Context, Principal, SystemFilter) ([]SystemRole, int64, error)
	ListSystemMenus(context.Context, Principal) ([]SystemMenu, error)
	ListSystemGroup(context.Context, Principal, int64) ([]SystemGroupMember, error)
	ApplySystemChange(context.Context, Principal, SystemChange) (any, error)
}

type SystemID int64

func (id *SystemID) UnmarshalJSON(raw []byte) error {
	value := string(raw)
	if strings.HasPrefix(value, "\"") {
		if err := json.Unmarshal(raw, &value); err != nil {
			return ErrSystemInvalid
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return ErrSystemInvalid
	}
	*id = SystemID(n)
	return nil
}

func (id SystemID) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatInt(int64(id), 10))
}

func (id SystemID) Value() int64 { return int64(id) }

type SystemAccount struct {
	ID            SystemID   `json:"id"`
	Username      string     `json:"username"`
	DisplayName   string     `json:"nickName"`
	Email         string     `json:"email"`
	Phone         string     `json:"phoneNumber"`
	Gender        int16      `json:"gender"`
	Type          int16      `json:"ty"`
	AgencyLevel   int32      `json:"agencyLevel"`
	Admin         bool       `json:"-"`
	Disabled      bool       `json:"-"`
	Deleted       bool       `json:"-"`
	Remark        string     `json:"remark"`
	Created       time.Time  `json:"createTime"`
	RoleIDs       []SystemID `json:"roleIds"`
	UserPrivilege []SystemID `json:"userPrivilege"`
	RoleName      string     `json:"roleName"`
	ManagerName   string     `json:"managerName"`
	Credential    string     `json:"-"`
	PasswordHash  string     `json:"-"`
}

type SystemRole struct {
	ID          SystemID   `json:"id"`
	Name        string     `json:"name"`
	Code        string     `json:"code"`
	Sort        int32      `json:"sort"`
	Status      int16      `json:"status"`
	Remark      string     `json:"remark"`
	Created     time.Time  `json:"createTime"`
	MenuIDs     []SystemID `json:"menuIds"`
	Staff       int64      `json:"staffNum"`
	ManagerName string     `json:"managerName"`
}

type SystemMenu struct {
	ID         SystemID  `json:"id"`
	Parent     SystemID  `json:"pid"`
	Name       string    `json:"name"`
	URL        string    `json:"url"`
	Type       int16     `json:"menuType"`
	Hidden     int16     `json:"hidden"`
	Target     int16     `json:"target"`
	Refresh    int16     `json:"refresh"`
	Permission string    `json:"perms"`
	Icon       string    `json:"icon"`
	Sort       int32     `json:"sort"`
	Remark     string    `json:"remark"`
	Created    time.Time `json:"createTime"`
}

type SystemTenant struct {
	ID      SystemID  `json:"id"`
	Name    string    `json:"name"`
	Code    string    `json:"code"`
	Remark  string    `json:"remark"`
	Created time.Time `json:"createTime"`
}

type SystemGroupMember struct {
	ID         SystemID `json:"userId"`
	Name       string   `json:"name"`
	Email      string   `json:"email"`
	GroupAdmin int      `json:"groupAdmin"`
}

type SystemFilter struct {
	PageRequest
	PageParams  *PageRequest `json:"pageParams"`
	ID          SystemID     `json:"id"`
	Username    string       `json:"username"`
	Name        string       `json:"name"`
	Type        *int16       `json:"ty"`
	RoleName    string       `json:"roleName"`
	ManagerName string       `json:"roleManageName"`
	ManageName  string       `json:"manageName"`
	Managers    bool         `json:"-"`
	All         bool         `json:"-"`
}

type SystemChange struct {
	Resource, Action, Permission string
	Account                      SystemAccount
	Role                         SystemRole
	Menu                         SystemMenu
	Tenant                       SystemTenant
	IDs                          []SystemID
	Member                       SystemID
	Status                       *int16
	Elevation                    bool
	OldPassword                  string
}

func NewDatabasePrincipal(area, scope string, account SystemAccount, roles []string, menus []SystemMenu) (Principal, error) {
	if account.Disabled || account.Deleted || account.Credential == "" {
		return Principal{}, ErrBackofficeNotFound
	}
	password, valid := auth.ParseLegacyPassword(account.PasswordHash)
	if !valid {
		return Principal{}, ErrBackofficeNotFound
	}
	p := Principal{ID: int64(account.ID), Area: area, Username: account.Username, Nickname: account.DisplayName,
		Email: account.Email, Admin: account.Admin, Roles: roles, password: password, DatabaseVersion: account.Credential}
	if area == MCNArea {
		id, err := strconv.ParseInt(scope, 10, 64)
		if err != nil || id <= 0 {
			return Principal{}, ErrBackofficeNotFound
		}
		p.AgencyID, p.Manager = id, !p.Admin && account.Type == 2
		p.AccountType = int(account.Type)
	} else {
		p.Tenant = scope
	}
	p.fingerprint = sha256.Sum256([]byte("database\x00" + area + "\x00" + scope + "\x00" + strconv.FormatInt(p.ID, 10) + "\x00" + account.Credential))
	for _, menu := range menus {
		if menu.Permission != "" {
			p.Permissions = append(p.Permissions, menu.Permission)
		}
	}
	slices.Sort(p.Permissions)
	p.Permissions = slices.Compact(p.Permissions)
	p.Routes = SystemRoutes(menus)
	return p, nil
}

func SystemRoutes(menus []SystemMenu) []Route {
	byParent := make(map[SystemID][]SystemMenu)
	for _, menu := range menus {
		if menu.URL != "" && menu.Hidden == 0 {
			byParent[menu.Parent] = append(byParent[menu.Parent], menu)
		}
	}
	var routes func(SystemID) []Route
	routes = func(id SystemID) []Route {
		list := []Route{}
		for _, menu := range byParent[id] {
			list = append(list, Route{Path: menu.URL, Name: menu.Name, Icon: menu.Icon, Routes: routes(menu.ID)})
		}
		return list
	}
	return routes(0)
}

func SystemPassword(password string) (string, error) {
	if len(password) < 6 || len(password) > 4096 {
		return "", ErrSystemInvalid
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt[:], 2, 19456, 1, 32)
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func (p Principal) Can(permission string) bool {
	return p.Admin || slices.Contains(p.Permissions, permission)
}

func (p Principal) MatchesSystemPassword(password string) bool {
	return len(password) <= 4096 && p.DatabaseVersion != "" && p.password.Matches(password)
}
