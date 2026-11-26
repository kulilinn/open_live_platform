package admin

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type pageNumber int64

func (p *pageNumber) UnmarshalJSON(value []byte) error {
	text := string(value)
	if len(value) > 0 && value[0] == '"' {
		if err := json.Unmarshal(value, &text); err != nil {
			return err
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 32)
	*p = pageNumber(parsed)
	return err
}

type PageRequest struct {
	Page pageNumber `json:"page"`
	Size pageNumber `json:"pageSize"`
}

func (p PageRequest) Window() (page, size, offset int64) {
	page, size = max(1, int64(p.Page)), int64(p.Size)
	if size <= 0 {
		size = 20
	}
	size = min(size, 1000)
	return page, size, (page - 1) * size
}

type UserQuery struct {
	PageRequest
	ID         *int64     `json:"id,string"`
	Nickname   *string    `json:"nickName"`
	Email      *string    `json:"email"`
	Status     *int       `json:"status"`
	LoginStart *string    `json:"loginStartTime"`
	LoginEnd   *string    `json:"loginEndTime"`
	Start, End *time.Time `json:"-"`
}

type UserRow struct {
	ID            int64      `json:"id,string"`
	Email         string     `json:"email"`
	DisplayName   string     `json:"displayName"`
	SearchName    *string    `json:"searchName"`
	Photo         *string    `json:"photo"`
	Gender        *int       `json:"gender"`
	CreatedAt     time.Time  `json:"createAt"`
	LastLogin     *time.Time `json:"lastLoginTime"`
	Point         *int64     `json:"point"`
	UsedPoint     *int64     `json:"totalUsedPoint"`
	ExpiredPoint  *int64     `json:"totalExpiredPoint"`
	HistoryPoints *int64     `json:"historyTotal"`
	IsStreamer    int        `json:"isStreamer"`
	Status        int        `json:"status"`
	RoomLimited   *int       `json:"roomLimited"`
	RoomLimitedAt *time.Time `json:"roomLimitedTime"`
}

type RoomQuery struct {
	PageRequest
	StreamerID *int64  `json:"streamerId,string"`
	RoomID     *int64  `json:"roomId,string"`
	Name       *string `json:"streamerName"`
	Email      *string `json:"email"`
}

type RoomRow struct {
	RoomID        int64      `json:"roomId,string"`
	ChatID        int64      `json:"chatId,string"`
	LiveID        int64      `json:"liveId,string"`
	StreamerID    int64      `json:"streamerId,string"`
	Name          string     `json:"streamerName"`
	Gender        *int       `json:"gender"`
	OnlineTime    int64      `json:"onlineTime"`
	RoomType      string     `json:"roomType"`
	Ticket        string     `json:"ticket"`
	Members       int        `json:"roomGuestNum"`
	Gifts         int64      `json:"giftNum"`
	Online        int        `json:"onlineStatus"`
	Limited       int        `json:"limited"`
	LimitedAt     *time.Time `json:"limitedTime"`
	Banner        string     `json:"banner"`
	LiveDuration  int64      `json:"liveDuration"`
	LiveTime      *time.Time `json:"liveTime"`
	AccountStatus int        `json:"accountStatus"`
	AccountLimit  int64      `json:"limitDuration"`
}

type Action struct {
	Kind, Path               string
	UserID, RoomID, Duration int64
	Reason                   []string
}

type Violation struct {
	UserID    int64     `json:"userId,string"`
	Reason    string    `json:"reason"`
	Duration  int64     `json:"limitDuration"`
	Type      int       `json:"ty"`
	Created   time.Time `json:"createTime"`
	ActorName string    `json:"createByName"`
}

type Log struct {
	Target    string    `json:"target"`
	Action    string    `json:"action"`
	ActorID   int64     `json:"createBy,string"`
	ActorName string    `json:"createByName"`
	Created   time.Time `json:"createTime"`
}

type StreamerQuery struct {
	PageRequest
	Name      *string `json:"name"`
	Email     *string `json:"email"`
	ManagerID *int64  `json:"managerId,string"`
	Status    []int16 `json:"status"`
	Date      *string `json:"date"`
	All       bool    `json:"-"`
}

type StreamerRow struct {
	ID           int64     `json:"id,string"`
	StreamerID   int64     `json:"streamerId,string"`
	ManagerID    *int64    `json:"managerId,string"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	ManagerName  *string   `json:"managerName"`
	Ratio        string    `json:"ratio"`
	ManagerRatio string    `json:"managerRatio"`
	AgencyRatio  string    `json:"agencyRatio"`
	Fans         *int      `json:"fanNum"`
	Status       int       `json:"status"`
	Created      time.Time `json:"createTime"`
}

func backofficePage[T any](w http.ResponseWriter, list []T, query PageRequest, total int64, err error) {
	page, size, _ := query.Window()
	backofficeReply(w, map[string]any{"list": nonNil(list), "current": page, "pageSize": size, "total": total}, err)
}

func (b backoffice) registerOperations(mux *http.ServeMux) {
	info := b.authorized(false, false, func(w http.ResponseWriter, _ *http.Request, p Principal, _ [sha256.Size]byte) {
		backofficeReply(w, map[string]any{"id": strconv.FormatInt(p.ID, 10), "username": p.Username, "nickName": p.Nickname,
			"admin": p.Admin, "roles": nonNil(p.Roles), "permissions": nonNil(p.Permissions), "routes": nonNil(p.Routes)}, nil)
	})
	mux.HandleFunc("POST /api/system/user/info", info)
	mux.HandleFunc("POST /api/system/user/routes", b.authorized(false, true, func(w http.ResponseWriter, _ *http.Request, p Principal, _ [sha256.Size]byte) {
		backofficeReply(w, nonNil(p.Routes), nil)
	}))
	add := func(path string, handle backofficeHandler) {
		mux.HandleFunc("POST "+path, b.authorized(false, true, handle))
	}
	add("/api/operation/usermanage/user/page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query UserQuery
		if !httpx.Decode(w, r, &query) {
			return
		}
		var ok bool
		query.Start, query.End, ok = backofficeDateRange(query.LoginStart, query.LoginEnd)
		if !ok {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid login time range")
			return
		}
		list, total, err := b.store.BackofficeUsers(r.Context(), query)
		backofficePage(w, list, query.PageRequest, total, err)
	})
	add("/api/operation/usermanage/user/detail", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var body struct {
			Account int64 `json:"account,string"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if body.Account <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "invalid account")
			return
		}
		list, _, err := b.store.BackofficeUsers(r.Context(), UserQuery{ID: &body.Account})
		if err != nil || len(list) == 0 {
			if err == nil {
				err = ErrBackofficeNotFound
			}
			backofficeReply(w, nil, err)
			return
		}
		u := list[0]
		backofficeReply(w, map[string]any{"id": strconv.FormatInt(u.ID, 10), "email": u.Email, "name": u.DisplayName,
			"photo": u.Photo, "gender": u.Gender, "isStreamer": u.IsStreamer, "createAt": u.CreatedAt}, nil)
	})
	for _, kind := range []string{"account_disable", "account_enable", "account_reset", "no_talking"} {
		add("/api/operation/usermanage/user/"+kind, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var body struct {
				ID       int64    `json:"userId,string"`
				Reason   []string `json:"reason"`
				Duration *int64   `json:"periodTime"`
			}
			if !httpx.Decode(w, r, &body) {
				return
			}
			duration := int64(0)
			if kind == "account_disable" {
				duration = -1
			}
			if body.Duration != nil {
				duration = *body.Duration
			}
			if body.ID <= 0 || duration < -1 || duration > math.MaxInt32 || (kind == "no_talking" && duration == 0) {
				backofficeError(w, http.StatusBadRequest, "400", "invalid account action")
				return
			}
			if kind == "account_disable" && duration == 0 {
				duration = -1
			}
			backofficeReply(w, nil, b.store.ApplyBackofficeAction(r.Context(), p,
				Action{Kind: kind, Path: r.URL.Path, UserID: body.ID, Duration: duration, Reason: body.Reason}))
		})
	}
	add("/api/operation/usermanage/user/violation_records", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query struct {
			PageRequest
			ID int64 `json:"id,string"`
		}
		if !httpx.Decode(w, r, &query) {
			return
		}
		if query.ID <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "invalid account")
			return
		}
		list, total, err := b.store.BackofficeViolations(r.Context(), query.ID, query.PageRequest)
		backofficePage(w, list, query.PageRequest, total, err)
	})
	add("/api/system/user/operationlogpage", func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var query PageRequest
		if !httpx.Decode(w, r, &query) {
			return
		}
		list, total, err := b.store.BackofficeLogs(r.Context(), p, query)
		backofficePage(w, list, query, total, err)
	})
	add("/api/operation/room/page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var query RoomQuery
		if !httpx.Decode(w, r, &query) {
			return
		}
		list, total, err := b.store.BackofficeRooms(r.Context(), query)
		backofficePage(w, list, query.PageRequest, total, err)
	})
	add("/api/operation/room/streamer_op", func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body struct {
			Kind   string   `json:"ty"`
			ID     int64    `json:"streamerId,string"`
			Room   int64    `json:"roomId,string"`
			Reason []string `json:"reason"`
			Limit  *int64   `json:"limitTime"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		kind := map[string]string{"EndStream": "streamer_end_stream", "Disable": "streamer_disable", "Enable": "streamer_enable", "Alert": "streamer_alert"}[body.Kind]
		duration := int64(0)
		if body.Kind == "Disable" {
			duration = -1
		}
		if body.Limit != nil {
			duration = *body.Limit
		}
		if kind == "" || body.ID <= 0 || body.Room <= 0 || duration < -1 || duration > math.MaxInt32 || (body.Kind == "Disable" && duration == 0) {
			backofficeError(w, http.StatusBadRequest, "400", "invalid streamer action")
			return
		}
		if body.Kind == "Alert" {
			backofficeError(w, http.StatusServiceUnavailable, "503", "streamer alert delivery unavailable")
			return
		}
		backofficeReply(w, nil, b.store.ApplyBackofficeAction(r.Context(), p,
			Action{Kind: kind, Path: r.URL.Path, UserID: body.ID, RoomID: body.Room, Duration: duration, Reason: body.Reason}))
	})
}

func (b backoffice) registerMCN(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/system/user/get_routes", b.authorized(false, false, func(w http.ResponseWriter, _ *http.Request, p Principal, _ [sha256.Size]byte) {
		backofficeReply(w, map[string]any{"id": strconv.FormatInt(p.ID, 10), "email": p.Email, "superAdmin": p.Admin,
			"permissions": nonNil(p.Permissions), "routes": nonNil(p.Routes), "lrAdmin": false}, nil)
	}))
	for _, action := range []string{"list", "page"} {
		mux.HandleFunc("POST /api/streamer/"+action, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var query StreamerQuery
			if !httpx.Decode(w, r, &query) {
				return
			}
			if query.Date != nil {
				if _, err := time.Parse("2006-01-02", strings.TrimSpace(*query.Date)); err != nil {
					backofficeError(w, http.StatusBadRequest, "400", "invalid streamer date")
					return
				}
			}
			if p.Manager {
				query.ManagerID = &p.ID
			}
			query.All = action == "list"
			list, total, err := b.store.BackofficeStreamers(r.Context(), p, query)
			if !query.All {
				backofficePage(w, list, query.PageRequest, total, err)
				return
			}
			data := make([]map[string]string, 0, len(list))
			for _, row := range list {
				data = append(data, map[string]string{"id": strconv.FormatInt(row.ID, 10), "streamerId": strconv.FormatInt(row.StreamerID, 10), "name": row.Name})
			}
			backofficeReply(w, data, err)
		}))
	}
}
