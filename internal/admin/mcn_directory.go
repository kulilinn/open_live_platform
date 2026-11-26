package admin

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"open_live_platform/internal/transport/httpx"
)

type MCNAgencyFilter struct {
	PageRequest
	Name      *string `json:"name"`
	SuperName *string `json:"superName"`
}

type MCNAgencyRow struct {
	ID           int64   `json:"mcnId,string"`
	Name         string  `json:"agencyName"`
	SuperName    string  `json:"superName"`
	SuperEmail   string  `json:"superEmail"`
	TemplateID   *int64  `json:"templateId,string"`
	TemplateName *string `json:"templateName"`
	Streamers    int64   `json:"streamerNum"`
	Staff        int64   `json:"staffNum"`
}

type MCNAgencyEdit struct {
	ID          int64  `json:"id,string"`
	Name        string `json:"name"`
	SuperName   string `json:"superName"`
	SuperEmail  string `json:"email"`
	Password    string `json:"password"`
	AgencyLevel int    `json:"agencyLevel"`
	TemplateID  *int64 `json:"sharingTemplateId,string"`
}

type MCNAgencyStreamerQuery struct {
	AgencyID   int64   `json:"mcnId,string"`
	StreamerID *int64  `json:"streamerId,string"`
	Name       *string `json:"streamerName"`
}

type MCNAgencyStreamer struct {
	AgencyID   int64      `json:"mcnId,string"`
	StreamerID int64      `json:"streamerId,string"`
	Name       string     `json:"streamerName"`
	Email      string     `json:"streamerEmail"`
	JoinedAt   *time.Time `json:"joinTime"`
	CreatedAt  time.Time  `json:"createTime"`
}

type MCNTemplateQuery struct {
	AgencyID   int64 `json:"mcnId,string"`
	TemplateID int64 `json:"templateId,string"`
}

type MCNTemplate struct {
	ID   int64  `json:"id,string"`
	Name string `json:"name"`
}

type MCNTemplateTier struct {
	MCNTemplate
	Min   int64  `json:"min"`
	Max   int64  `json:"max"`
	Ratio string `json:"ratio"`
}

type MCNTemplateEdit struct {
	ID       *int64     `json:"id,string"`
	AgencyID int64      `json:"mcnId,string"`
	Name     string     `json:"name"`
	Data     [][]uint32 `json:"data"`
}

type MCNDirectoryStore interface {
	MCNAgencies(context.Context, MCNAgencyFilter) ([]MCNAgencyRow, int64, error)
	MCNAgencyStreamers(context.Context, MCNAgencyStreamerQuery) ([]MCNAgencyStreamer, error)
	WriteMCNAgency(ctx context.Context, actor Principal, path, action string, record MCNAgencyEdit, passwordHash string) error
	MCNTemplates(context.Context) ([]MCNTemplate, error)
	MCNTemplateTiers(context.Context, MCNTemplateQuery) ([]MCNTemplateTier, error)
	WriteMCNTemplate(ctx context.Context, actor Principal, path string, record MCNTemplateEdit, remove bool) error
}

func (b backoffice) registerMCNDirectory(mux *http.ServeMux) {
	store, ok := b.store.(MCNDirectoryStore)
	if !ok {
		return
	}
	b.mountMCNFinance(mux, "/agency/page", "/agency/organize/page", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q MCNAgencyFilter
		if httpx.Decode(w, r, &q) {
			rows, total, err := store.MCNAgencies(r.Context(), q)
			backofficePage(w, rows, q.PageRequest, total, err)
		}
	})
	b.mountMCNFinance(mux, "/agency/streamer", "/agency/organize/streamer", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q MCNAgencyStreamerQuery
		if httpx.Decode(w, r, &q) {
			if q.AgencyID <= 0 || (q.StreamerID != nil && *q.StreamerID <= 0) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid agency or streamer")
				return
			}
			rows, err := store.MCNAgencyStreamers(r.Context(), q)
			backofficeReply(w, nonNil(rows), err)
		}
	})
	for _, op := range []string{"add", "edit", "delete"} {
		b.mountMCNFinance(mux, "/agency/"+op, "/agency/organize/"+op, func(w http.ResponseWriter, r *http.Request, operator Principal, _ [sha256.Size]byte) {
			var q MCNAgencyEdit
			if !httpx.Decode(w, r, &q) {
				return
			}
			hash, err := validateMCNAgency(&q, op)
			if err == nil {
				err = store.WriteMCNAgency(r.Context(), operator, r.URL.Path, op, q, hash)
			}
			mcnDirectoryResult(w, err)
		})
	}
	b.mountMCNFinance(mux, "/template/page", "/agency/template/list", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		rows, err := store.MCNTemplates(r.Context())
		backofficeReply(w, nonNil(rows), err)
	})
	b.mountMCNFinance(mux, "/template/detail", "/agency/template/detail", func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
		var q MCNTemplateQuery
		if httpx.Decode(w, r, &q) {
			if q.AgencyID < 0 || q.TemplateID <= 0 {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid template")
				return
			}
			rows, err := store.MCNTemplateTiers(r.Context(), q)
			backofficeReply(w, nonNil(rows), err)
		}
	})
	writeTemplate := func(w http.ResponseWriter, r *http.Request, operator Principal, _ [sha256.Size]byte) {
		var q MCNTemplateEdit
		if !httpx.Decode(w, r, &q) {
			return
		}
		remove := strings.HasSuffix(r.URL.Path, "/delete")
		if !validateMCNTemplate(&q, remove) {
			mcnDirectoryResult(w, ErrSystemInvalid)
			return
		}
		mcnDirectoryResult(w, store.WriteMCNTemplate(r.Context(), operator, r.URL.Path, q, remove))
	}
	b.mountMCNFinance(mux, "/template/add", "/agency/template/add_edit", writeTemplate)
	b.mountMCNFinance(mux, "/template/edit", "", writeTemplate)
	b.mountMCNFinance(mux, "/template/delete", "/agency/template/delete", writeTemplate)
}

func (b backoffice) mountMCNFinance(mux *http.ServeMux, finance, internal string, next backofficeHandler) {
	if b.area == OperationArea && finance != "" {
		mux.HandleFunc("POST /api/finance"+finance, b.authorizedTenant(false, true, FinanceTenant, next))
		return
	}
	if b.area != MCNArea || internal == "" || b.options.MCNInternalToken == "" {
		return
	}
	mux.HandleFunc("POST /api"+internal, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Header.Get("X-Inner-Service") != "admin" || subtle.ConstantTimeCompare([]byte(raw), []byte(b.options.MCNInternalToken)) != 1 {
			backofficeError(w, http.StatusUnauthorized, "401", "Trusted finance service is required")
			return
		}
		next(w, r, Principal{Area: OperationArea, Tenant: FinanceTenant, Username: "admin-service", Admin: true}, [sha256.Size]byte{})
	})
}

func validateMCNAgency(entry *MCNAgencyEdit, action string) (string, error) {
	if action == "add" {
		entry.ID = 0
	}
	if (action != "add" && entry.ID <= 0) || (entry.TemplateID != nil && *entry.TemplateID <= 0) {
		return "", ErrSystemInvalid
	}
	if action == "delete" {
		return "", nil
	}
	entry.Name = strings.TrimSpace(entry.Name)
	if entry.Name == "" || len(entry.Name) > 128 {
		return "", ErrSystemInvalid
	}
	if action == "edit" {
		return "", nil
	}
	entry.SuperName = strings.TrimSpace(entry.SuperName)
	entry.SuperEmail = strings.ToLower(strings.TrimSpace(entry.SuperEmail))
	addr, err := mail.ParseAddress(entry.SuperEmail)
	if err != nil || addr.Address != entry.SuperEmail || len(entry.SuperEmail) > 128 || entry.SuperName == "" || len(entry.SuperName) > 128 || entry.AgencyLevel < 0 {
		return "", ErrSystemInvalid
	}
	return SystemPassword(entry.Password)
}

func validateMCNTemplate(entry *MCNTemplateEdit, remove bool) bool {
	if entry.AgencyID < 0 || (entry.ID != nil && *entry.ID <= 0) {
		return false
	}
	if remove {
		return entry.ID != nil
	}
	entry.Name = strings.TrimSpace(entry.Name)
	if entry.Name == "" || len(entry.Name) > 128 || len(entry.Data) == 0 || len(entry.Data) > 100 {
		return false
	}
	for _, tier := range entry.Data {
		if len(tier) != 3 || tier[0] > tier[1] || tier[2] > 100 {
			return false
		}
	}
	slices.SortFunc(entry.Data, func(a, b []uint32) int { return cmp.Compare(a[0], b[0]) })
	for i := 1; i < len(entry.Data); i++ {
		if entry.Data[i][0] <= entry.Data[i-1][1] {
			return false
		}
	}
	return true
}

func mcnDirectoryResult(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSystemConflict):
		backofficeError(w, http.StatusConflict, "409", "Agency or template already exists or is in use")
	case errors.Is(err, ErrSystemInvalid):
		backofficeError(w, http.StatusBadRequest, "400", "Invalid agency or template")
	default:
		mcnOperationResult(w, err)
	}
}
