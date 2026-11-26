package admin

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"

	"open_live_platform/internal/catalog"
	"open_live_platform/internal/transport/httpx"
)

type withdrawalSettings struct {
	Ratio  string `json:"withdrawal_fee_ratio"`
	Fee    string `json:"min_withdrawal_fee"`
	Amount string `json:"min_withdrawal_amount"`
}

var feeRatio = regexp.MustCompile(`^[0-9]+(?:\.0+)?$`)
var feeAmount = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

func (b backoffice) registerPlatformSettings(mux *http.ServeMux) {
	editor, ok := b.store.(catalogEditor)
	if !ok {
		return
	}
	register := func(path, key, tenant string, write bool) {
		op := CatalogOperation{Path: path, Sections: []string{"common_settings"}, Write: write}
		mux.HandleFunc("POST "+path, b.authorizedTenant(false, true, tenant, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var input struct{ Content *string }
			finance := key == "withdrawal_settings"
			if write && finance {
				var f withdrawalSettings
				if !httpx.Decode(w, r, &f) {
					return
				}
				if !feeRatio.MatchString(f.Ratio) || !feeAmount.MatchString(f.Fee) || !feeAmount.MatchString(f.Amount) {
					backofficeError(w, 400, "400", "Invalid withdrawal settings")
					return
				}
				raw, _ := json.Marshal(f)
				s := string(raw)
				input.Content = &s
			} else if write {
				if !httpx.Decode(w, r, &input) {
					return
				}
				if input.Content == nil {
					backofficeError(w, 400, "400", "Missing setting content")
					return
				}
			}
			result, err := editor.OperateCatalog(r.Context(), p, op, func(d catalog.Snapshot, _ int64) (any, error) {
				rows := d["common_settings"]
				i := slices.IndexFunc(rows, func(r catalog.Row) bool { return r.String("id") == key })
				var content string
				if i >= 0 {
					content = rows[i].String("content")
				}
				if write {
					content = *input.Content
					if i < 0 {
						rows = append(rows, catalog.Row{"id": value(key)})
						i = len(rows) - 1
					}
					rows[i]["content"] = value(content)
					d["common_settings"] = rows
				}
				if !finance {
					return map[string]string{"content": content}, nil
				}
				f := withdrawalSettings{"0", "0", "0"}
				if content != "" {
					if err := json.Unmarshal([]byte(content), &f); err != nil {
						return nil, err
					}
				}
				return map[string]string{"withdrawalFeeRatio": f.Ratio, "minWithdrawalFee": f.Fee, "minWithdrawalAmount": f.Amount}, nil
			})
			backofficeReply(w, result, err)
		}))
	}
	const black = "/api/operation/usermanage/black/"
	const white = "/api/operation/platform/whiteuser/"
	const withdraw = "/api/finance/withdrawalaudit/withdrawalsettings/"
	register(black+"getusers", "black_user", OperationTenant, false)
	register(black+"setusers", "black_user", OperationTenant, true)
	register(black+"getips", "black_ip", OperationTenant, false)
	register(black+"setips", "black_ip", OperationTenant, true)
	register(white+"get", "white_user", OperationTenant, false)
	register(white+"set", "white_user", OperationTenant, true)
	register(withdraw+"get", "withdrawal_settings", FinanceTenant, false)
	register(withdraw+"set", "withdrawal_settings", FinanceTenant, true)
}
