package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"open_live_platform/internal/collectible"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

const (
	MCNCheckWait   = 1
	MCNCheckPassed = 2
	MCNCheckReject = 3
	MCNJoinNormal  = 4
	MCNJoinWait    = 5
	MCNJoinFinish  = 6
	MCNNoRenewal   = 7
)

var ErrMCNState = errors.New("MCN application cannot make that transition")
var ErrMCNInviteUnavailable = errors.New("MCN invitation delivery is unavailable")
var ErrMCNReconcile = errors.New("MCN final settlement requires receipt reconciliation")
var ErrBackofficeForbidden = errors.New("operation not allowed")

type MCNRatio string

var ratioPattern = regexp.MustCompile(`^(?:0(?:\.[0-9]{1,8})?|1(?:\.0{1,8})?)$`)

func (ratio *MCNRatio) UnmarshalJSON(data []byte) error {
	value := string(data)
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
	}
	if !ratioPattern.MatchString(value) {
		return errors.New("ratio must be between 0 and 1")
	}
	*ratio = MCNRatio(value)
	return nil
}

type MCNEdit struct {
	ID           *int64    `json:"id,string"`
	Name         string    `json:"name"`
	Email        *string   `json:"email"`
	ManagerID    *int64    `json:"managerId,string"`
	StreamerID   *int64    `json:"streamerId,string"`
	Ratio        MCNRatio  `json:"ratio"`
	ManagerRatio *MCNRatio `json:"managerRatio"`
	AgencyRatio  *MCNRatio `json:"agencyRatio"`
}

type MCNOnboardingStore interface {
	SetMCNApplication(ctx context.Context, actor Principal, path, action, inviteBase string, edit MCNEdit, managerName *string, status int) error
	ConfirmMCNInvitation(ctx context.Context, mcnID, streamerID int64, hash [sha256.Size]byte) error
	FinishMCNMembership(ctx context.Context, actor Principal, path string, streamerID int64) error
}

type BackofficeOptions struct {
	MCNJoinURL, MCNInternalToken string
	ConfigDiagnostics            http.HandlerFunc
	Finance                      FinancePayments
	Images                       upload.Config
	ImagePrefix                  string
	Collectibles                 *collectible.Service
	LegacyWorkbookReader         string
}

func (b backoffice) registerMCNOnboarding(mux *http.ServeMux) {
	store, ok := b.store.(MCNOnboardingStore)
	if !ok {
		return
	}
	for _, action := range []string{"add", "edit", "add_confirm", "update_status", "account_new"} {
		mux.HandleFunc("POST /api/streamer/"+action, b.authorized(false, true,
			func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
				if !actor.Admin && !actor.Manager {
					backofficeError(w, http.StatusForbidden, "403", "Agency role is required")
					return
				}
				var edit MCNEdit
				var manager *string
				var status int
				if action == "update_status" || action == "account_new" {
					var input struct {
						ID         int64 `json:"id,string"`
						StreamerID int64 `json:"streamerId,string"`
						Status     int   `json:"status"`
					}
					if !httpx.Decode(w, r, &input) {
						return
					}
					edit.ID, edit.StreamerID, status = &input.ID, &input.StreamerID, input.Status
				} else {
					if !httpx.Decode(w, r, &edit) {
						return
					}
					edit.Name = strings.TrimSpace(edit.Name)
					if edit.Name == "" || len(edit.Name) > 240 || edit.Ratio == "" {
						backofficeError(w, http.StatusBadRequest, "400", "Invalid streamer application")
						return
					}
					var valid bool
					edit.ManagerID, manager, valid = b.manager(r.Context(), actor, edit.ManagerID)
					if !valid {
						backofficeError(w, http.StatusForbidden, "403", "Manager is outside the agency")
						return
					}
				}
				if (action != "add" && (edit.ID == nil || *edit.ID <= 0)) ||
					(action == "add" && (edit.Email == nil || strings.TrimSpace(*edit.Email) == "")) ||
					(action == "update_status" && status != MCNCheckPassed && status != MCNCheckReject) {
					backofficeError(w, http.StatusBadRequest, "400", "Invalid streamer application")
					return
				}
				if action != "add" && action != "edit" && !actor.Admin {
					backofficeError(w, http.StatusForbidden, "403", "Agency administrator is required")
					return
				}
				mcnOperationResult(w, store.SetMCNApplication(r.Context(), actor, r.URL.Path, action, b.options.MCNJoinURL, edit, manager, status))
			}))
	}
	mux.HandleFunc("GET /api/mcn/streamer/join/{mcn_id}/{streamer_id}/{sign_str}", func(w http.ResponseWriter, r *http.Request) {
		mcn, ok := httpx.PathID(w, r, "mcn_id")
		if !ok {
			return
		}
		streamer, ok := httpx.PathID(w, r, "streamer_id")
		if !ok {
			return
		}
		raw := r.PathValue("sign_str")
		if len(raw) < 32 || len(raw) > 120 {
			backofficeError(w, http.StatusForbidden, "403", "Invalid or expired invitation")
			return
		}
		err := store.ConfirmMCNInvitation(r.Context(), mcn, streamer, sha256.Sum256([]byte(raw)))
		mcnOperationResult(w, err)
	})
	mux.HandleFunc("POST /api/streamer/no_renewal", b.authorized(false, true,
		func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var input struct {
				ID int64 `json:"id,string"`
			}
			if !httpx.Decode(w, r, &input) {
				return
			}
			if input.ID <= 0 {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid streamer ID")
				return
			}
			if !actor.Admin {
				backofficeError(w, http.StatusForbidden, "403", "Agency administrator is required")
				return
			}
			mcnOperationResult(w, store.FinishMCNMembership(r.Context(), actor, r.URL.Path, input.ID))
		}))
}

func (b backoffice) manager(ctx context.Context, actor Principal, id *int64) (*int64, *string, bool) {
	if actor.Manager {
		if id != nil && *id != actor.ID {
			return nil, nil, false
		}
		name := mcnStaffName(actor)
		return &actor.ID, &name, true
	}
	if id == nil || *id == 0 {
		return nil, nil, true
	}
	p, err := b.currentPrincipal(ctx, *id)
	if err == nil && p.Area == MCNArea && p.AgencyID == actor.AgencyID && p.Manager && !p.Disabled {
		name := mcnStaffName(p)
		return id, &name, true
	}
	return nil, nil, false
}

func mcnStaffName(actor Principal) string {
	if actor.Nickname != "" {
		return actor.Nickname
	}
	return actor.Name()
}

func mcnOperationResult(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMCNState), errors.Is(err, ErrMCNReconcile):
		backofficeError(w, http.StatusConflict, "409", err.Error())
	case errors.Is(err, ErrMCNInviteUnavailable):
		backofficeError(w, http.StatusServiceUnavailable, "503", err.Error())
	case errors.Is(err, ErrBackofficeForbidden):
		backofficeError(w, http.StatusForbidden, "403", "Operation not allowed")
	default:
		backofficeReply(w, nil, err)
	}
}

func validateMCNInviteURL(raw string) error {
	if raw == "" {
		return nil
	}
	address, err := url.Parse(raw)
	if err != nil || address.Hostname() == "" || (address.Scheme != "http" && address.Scheme != "https") || address.User != nil ||
		address.RawQuery != "" || address.Fragment != "" {
		return errors.New("MCN_STREAMER_JOIN_URL must be a valid HTTP invitation prefix")
	}
	return nil
}
