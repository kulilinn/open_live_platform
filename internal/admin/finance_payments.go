package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

const (
	FinanceReserved  = "reserved"
	FinanceConfirmed = "confirmed"
	FinanceDeclined  = "declined"
	FinanceUnknown   = "unknown"
)

var ErrFinanceConflict = errors.New("payment request conflicts with an existing operation")

type FinanceAttempt struct {
	ID      int64
	State   string
	Receipt json.RawMessage
}

type FinanceIntent struct {
	Action, Key string
	UserID      int64
	Delta       int64
	Reason      string
	Hash        [sha256.Size]byte
	Request     json.RawMessage
}

type FinancePointChange struct {
	UserID      int64  `json:"userId,string"`
	Type        int8   `json:"ty"`
	Points      int64  `json:"point"`
	ExpiresDays int64  `json:"pointExpireDays"`
	DebtState   *int64 `json:"badDebtState"`
	Reason      string `json:"reason"`
}

type FinanceLargeOrderChange struct {
	UserID      int64           `json:"userId,string"`
	Order       string          `json:"orderNo"`
	Transaction string          `json:"transId"`
	Amount      json.RawMessage `json:"amount"`
	Points      int64           `json:"point"`
	ExpiresDays int64           `json:"pointExpireDay"`
	Remark      string          `json:"remark"`
}

type FinancePointLog struct {
	Users      string    `json:"users"`
	Points     int64     `json:"point"`
	OperatorID int64     `json:"createBy,string"`
	Operator   string    `json:"createByName"`
	CreatedAt  time.Time `json:"createTime"`
	Reason     string    `json:"reason"`
}

type FinanceDailyPurchase struct {
	ID            int64  `json:"id,string"`
	DAU           int64  `json:"dau"`
	FirstPurchase int64  `json:"firstPurchaseNum"`
	Purchasers    int64  `json:"purchaseUserNum"`
	PointTotal    int64  `json:"pointTotal"`
	SpendTotal    int64  `json:"consumePointTotal"`
	PlatformTotal int64  `json:"platformPointTotal"`
	Amount        string `json:"payAmount"`
	Receivable    string `json:"receivableAmount"`
	Day           string `json:"createTime"`
}

type FinanceReadQuery struct {
	PageRequest
	FromText *string    `json:"startTime"`
	ToText   *string    `json:"endTime"`
	From, To *time.Time `json:"-"`
}

type FinancePaymentStore interface {
	ReserveFinanceAttempt(ctx context.Context, actor Principal, input FinanceIntent) (FinanceAttempt, bool, error)
	CompleteFinanceAttempt(ctx context.Context, attemptID int64, state string, receipt json.RawMessage, updates []payment.Receipt) error
	FinancePointLog(ctx context.Context, query FinanceReadQuery) ([]FinancePointLog, int64, error)
	FinancePurchaseStats(ctx context.Context, query FinanceReadQuery) ([]FinanceDailyPurchase, int64, error)
	FinanceOrderOwner(ctx context.Context, order string, user int64) (bool, error)
	Find(context.Context, int64) (auth.User, bool, error)
}

type FinancePayments struct {
	Billing      *payment.Client
	Confirmation *payment.LargeOrderConfirmation
	Proxy        payment.Proxy
}

type financeOutcome struct {
	data json.RawMessage
	debt []payment.Receipt
}

func (b backoffice) registerFinancePayments(mux *http.ServeMux) {
	store, ok := b.store.(FinancePaymentStore)
	if !ok || b.area != OperationArea {
		return
	}
	mount := func(path string, next backofficeHandler) {
		mux.HandleFunc("POST /api/finance/"+path, b.authorizedTenant(false, true, FinanceTenant, next))
	}
	for _, action := range []string{"add", "sub"} {
		mount("purchase/point/"+action, func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
			var q FinancePointChange
			if !httpx.Decode(w, r, &q) {
				return
			}
			if q.UserID <= 0 || q.Points <= 0 || len(q.Reason) > 4096 || (action == "add" && !normalizeFinanceCredit(&q)) {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid point adjustment")
				return
			}
			client := b.options.Finance.Billing
			if client == nil {
				financeUnavailable(w)
				return
			}
			delta := q.Points
			if action == "sub" {
				delta = -delta
			}
			b.executeFinance(w, r, actor, store, "point_"+action, "", q.UserID, delta, q.Reason, q,
				func(ctx context.Context, id int64) (financeOutcome, error) {
					if action == "add" {
						data, err := client.FinanceCredit(ctx, payment.FinanceCredit{UserID: q.UserID, Points: q.Points,
							PointExpireDays: q.ExpiresDays, OrderID: id, BadDebtState: *q.DebtState})
						return financeOutcome{data: data}, err
					}
					receipt, err := client.Decrease(ctx, q.UserID, q.Points, "finance-"+strconv.FormatInt(id, 10))
					encoded, _ := json.Marshal(receipt)
					return financeOutcome{data: encoded}, err
				}, financeTypedReply(nil))
		})
	}
	mount("order/set_bad_debt", func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		var q struct {
			OrderID int64 `json:"orderId,string"`
			UserID  int64 `json:"userId,string"`
		}
		if !httpx.Decode(w, r, &q) {
			return
		}
		if q.OrderID <= 0 || q.UserID <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid user or order")
			return
		}
		client := b.options.Finance.Billing
		if client == nil {
			financeUnavailable(w)
			return
		}
		b.executeFinance(w, r, actor, store, "bad_debt", strconv.FormatInt(q.OrderID, 10), q.UserID, 0, "", q,
			func(ctx context.Context, _ int64) (financeOutcome, error) {
				updates, err := client.FlagBadDebt(ctx, q.UserID, q.OrderID)
				data, _ := json.Marshal(updates)
				return financeOutcome{data: data, debt: updates}, err
			}, financeTypedReply(nil))
	})
	mount("order/large/add_point", func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		var q FinanceLargeOrderChange
		if !httpx.Decode(w, r, &q) {
			return
		}
		amount, valid := exactPayAmount(q.Amount)
		if q.UserID <= 0 || q.Points <= 0 || q.ExpiresDays < 0 || !valid || !financeOrderText(q.Order) || !financeOrderText(q.Transaction) || len(q.Remark) > 4096 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid large order credit")
			return
		}
		q.Amount, _ = json.Marshal(amount)
		client := b.options.Finance.Billing
		if client == nil {
			financeUnavailable(w)
			return
		}
		b.executeFinance(w, r, actor, store, "large_credit", q.Order, q.UserID, q.Points, q.Remark, q,
			func(ctx context.Context, _ int64) (financeOutcome, error) {
				data, err := client.CreditLargeOrder(ctx, payment.LargeOrderCredit{UserID: q.UserID, Number: q.Order,
					TransactionID: q.Transaction, PayAmount: amount, Points: q.Points, PointExpireDays: q.ExpiresDays, Remark: q.Remark, OperatorID: actor.ID})
				return financeOutcome{data: data}, err
			}, financeTypedReply(struct{}{}))
	})
	mount("order/large/second_send", b.financeSecondSend(store))
	for _, daily := range []bool{false, true} {
		path := "purchase/point/page"
		if daily {
			path = "purchase/statistic/today"
		}
		mount(path, func(w http.ResponseWriter, r *http.Request, _ Principal, _ [sha256.Size]byte) {
			var q FinanceReadQuery
			if !httpx.Decode(w, r, &q) {
				return
			}
			var valid bool
			q.From, q.To, valid = backofficeDateRange(q.FromText, q.ToText)
			if !valid {
				backofficeError(w, http.StatusBadRequest, "400", "Invalid report period")
				return
			}
			if daily {
				rows, total, err := store.FinancePurchaseStats(r.Context(), q)
				backofficePage(w, rows, q.PageRequest, total, err)
				return
			}
			rows, total, err := store.FinancePointLog(r.Context(), q)
			backofficePage(w, rows, q.PageRequest, total, err)
		})
	}
	b.registerFinanceProxy(mux, store)
}

func normalizeFinanceCredit(q *FinancePointChange) bool {
	if q.Type < 1 || q.Type > 6 || q.ExpiresDays < 0 {
		return false
	}
	state := int64(1)
	if q.DebtState != nil {
		state = *q.DebtState
	}
	if q.Type == 2 || q.Type == 3 {
		state = 2
	}
	q.DebtState = &state
	return state >= 0 && state <= 2
}

var payAmountPattern = regexp.MustCompile(`^[0-9]{1,24}(?:\.[0-9]{1,8})?$`)

func exactPayAmount(raw json.RawMessage) (string, bool) {
	amount := string(raw)
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &amount) != nil {
		return "", false
	}
	if !payAmountPattern.MatchString(amount) || !strings.ContainsAny(amount, "123456789") {
		return "", false
	}
	whole, fraction, _ := strings.Cut(amount, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return whole, true
	}
	return whole + "." + fraction, true
}

func financeOrderText(raw string) bool {
	return raw != "" && len(raw) <= 128 && !strings.ContainsAny(raw, " \t\r\n\x00")
}

func (b backoffice) financeSecondSend(store FinancePaymentStore) backofficeHandler {
	return func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
		var q struct {
			Number int64 `json:"no,string"`
			UserID int64 `json:"userId,string"`
		}
		if !httpx.Decode(w, r, &q) {
			return
		}
		if q.UserID <= 0 || q.Number <= 0 {
			backofficeError(w, http.StatusBadRequest, "400", "Invalid order or customer")
			return
		}
		if b.options.Finance.Confirmation == nil {
			financeUnavailable(w)
			return
		}
		number := strconv.FormatInt(q.Number, 10)
		known, err := store.FinanceOrderOwner(r.Context(), number, q.UserID)
		if !known || err != nil {
			if err == nil {
				err = ErrBackofficeNotFound
			}
			backofficeReply(w, nil, err)
			return
		}
		user, found, err := store.Find(r.Context(), q.UserID)
		if !found || err != nil {
			if err == nil {
				err = ErrBackofficeNotFound
			}
			backofficeReply(w, nil, err)
			return
		}
		search := user.Account
		if user.Profile != nil {
			search = user.Profile.SearchName
		}
		b.executeFinance(w, r, actor, store, "large_confirmation", "", q.UserID, 0, "", q,
			func(ctx context.Context, _ int64) (financeOutcome, error) {
				return financeOutcome{}, b.options.Finance.Confirmation.Notify(ctx, user.Email, user.Account, search, number)
			}, financeTypedReply(nil))
	}
}

func (b backoffice) executeFinance(w http.ResponseWriter, r *http.Request, actor Principal, store FinancePaymentStore,
	action, naturalKey string, userID, delta int64, reason string, input any, perform func(context.Context, int64) (financeOutcome, error),
	respond func(http.ResponseWriter, FinanceAttempt)) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !financeOrderText(key) {
		backofficeError(w, http.StatusBadRequest, "400", "Invalid Idempotency-Key")
		return
	}
	if key == "" {
		key = rand.Text()
	}
	r.Header.Set("Idempotency-Key", key)
	requestKey := key
	if naturalKey != "" {
		requestKey = naturalKey
	}
	w.Header().Set("Idempotency-Key", key)
	body, err := json.Marshal(input)
	if err != nil {
		backofficeReply(w, nil, err)
		return
	}
	attempt, created, err := store.ReserveFinanceAttempt(r.Context(), actor, FinanceIntent{Action: action,
		Key: requestKey, UserID: userID, Delta: delta, Reason: reason, Hash: sha256.Sum256(body), Request: body})
	if errors.Is(err, ErrFinanceConflict) {
		backofficeError(w, http.StatusConflict, "409", err.Error())
		return
	}
	if err != nil {
		backofficeReply(w, nil, err)
		return
	}
	w.Header().Set("X-Finance-Attempt", strconv.FormatInt(attempt.ID, 10))
	if !created {
		respond(w, attempt)
		return
	}
	outcome, effectErr := perform(r.Context(), attempt.ID)
	state := FinanceConfirmed
	if errors.Is(effectErr, payment.ErrInsufficientPoints) {
		state = FinanceDeclined
	} else if effectErr != nil {
		state = FinanceUnknown
	}
	if state != FinanceConfirmed {
		outcome = financeOutcome{}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	if err := store.CompleteFinanceAttempt(ctx, attempt.ID, state, outcome.data, outcome.debt); err != nil {
		_ = store.CompleteFinanceAttempt(ctx, attempt.ID, FinanceUnknown, outcome.data, nil)
		respond(w, FinanceAttempt{ID: attempt.ID, State: FinanceUnknown})
		return
	}
	attempt.State, attempt.Receipt = state, outcome.data
	respond(w, attempt)
}

func financeTypedReply(value any) func(http.ResponseWriter, FinanceAttempt) {
	return func(w http.ResponseWriter, attempt FinanceAttempt) {
		switch attempt.State {
		case FinanceConfirmed:
			backofficeReply(w, value, nil)
		case FinanceDeclined:
			backofficeError(w, http.StatusConflict, "409", "Insufficient points")
		default:
			backofficeError(w, http.StatusAccepted, "202", "Payment result requires reconciliation")
		}
	}
}

func financeUnavailable(w http.ResponseWriter) {
	backofficeError(w, http.StatusServiceUnavailable, "503", "Payment service is unavailable")
}

type financeProxyResult struct {
	Code   int         `json:"code"`
	Header http.Header `json:"headers"`
	Body   []byte      `json:"body"`
}

func (b backoffice) registerFinanceProxy(mux *http.ServeMux, store FinancePaymentStore) {
	proxy := b.options.Finance.Proxy
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		mux.HandleFunc(method+" /api/payment/{path...}", b.authorizedTenant(false, true, FinanceTenant,
			func(w http.ResponseWriter, r *http.Request, actor Principal, _ [sha256.Size]byte) {
				if r.Method != method || !proxy.Allows(r) {
					backofficeError(w, http.StatusNotFound, "404", "Payment route is not configured")
					return
				}
				perform := func(request *http.Request) (financeProxyResult, error) {
					response, err := proxy.Forward(request, actor.ID)
					if err != nil {
						return financeProxyResult{}, err
					}
					defer response.Body.Close()
					const limit = 1 << 20
					payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
					if err != nil || len(payload) > limit {
						return financeProxyResult{}, payment.ErrUnavailable
					}
					headers := http.Header{}
					for _, key := range []string{"Content-Type", "Content-Disposition", "Location", "Retry-After"} {
						if value := response.Header.Get(key); value != "" {
							headers.Set(key, value)
						}
					}
					return financeProxyResult{Code: response.StatusCode, Header: headers, Body: payload}, nil
				}
				if method == http.MethodGet {
					value, err := perform(r)
					if err != nil {
						backofficeError(w, http.StatusBadGateway, "502", "Payment provider unavailable")
						return
					}
					writeFinanceProxy(w, value)
					return
				}
				input, err := io.ReadAll(r.Body)
				if err != nil {
					backofficeError(w, http.StatusBadRequest, "400", "Invalid payment request")
					return
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(input))
				q := map[string]string{"path": r.URL.Path, "queryHash": fmt.Sprintf("%x", sha256.Sum256([]byte(r.URL.RawQuery))),
					"contentType": r.Header.Get("Content-Type"), "bodyHash": digest}
				b.executeFinance(w, r, actor, store, "proxy:"+actor.Scope()+":"+strconv.FormatInt(actor.ID, 10), "", actor.ID, 0, "", q,
					func(ctx context.Context, _ int64) (financeOutcome, error) {
						request := r.Clone(ctx)
						request.Body = io.NopCloser(bytes.NewReader(input))
						value, err := perform(request)
						if err != nil || value.Code >= 500 {
							return financeOutcome{}, payment.ErrUnavailable
						}
						data, _ := json.Marshal(value)
						return financeOutcome{data: data}, nil
					}, func(w http.ResponseWriter, attempt FinanceAttempt) {
						var value financeProxyResult
						if attempt.State != FinanceConfirmed || json.Unmarshal(attempt.Receipt, &value) != nil {
							financeTypedReply(nil)(w, attempt)
							return
						}
						writeFinanceProxy(w, value)
					})
			}))
	}
}

func writeFinanceProxy(w http.ResponseWriter, value financeProxyResult) {
	for key, values := range value.Header {
		w.Header()[key] = values
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(value.Code)
	_, _ = w.Write(value.Body)
}
