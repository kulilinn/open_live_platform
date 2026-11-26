package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/payment"
	"open_live_platform/internal/transport/httpx"
)

func RegisterPayment(mux *http.ServeMux, accounts auth.AccountStore, store payment.Store, proxy payment.Proxy, largeOrders *payment.LargeOrderService) {
	s := server{accounts: accounts}
	mux.HandleFunc("GET /api/payment/check/get", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		checks, err := store.PaymentChecks(r.Context(), id)
		if err != nil {
			writeGiftError(w, http.StatusInternalServerError, "500", "could not read payment checks")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "200", "data": checks})
	}))
	mux.HandleFunc("GET /api/payment/check/edit", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		params, err := url.ParseQuery(r.URL.RawQuery)
		confirm, confirmOK := paymentCheck(params, "confirm_check", true)
		adult, adultOK := paymentCheck(params, "adult_check", true)
		pay, payOK := paymentCheck(params, "confirm_pay_check", false)
		country, countryOK := paymentCheck(params, "confirm_country_check", false)
		if err != nil || !confirmOK || !adultOK || !payOK || !countryOK {
			writeGiftError(w, http.StatusBadRequest, "400", "invalid payment checks")
			return
		}
		checks := payment.Checks{Confirm: confirm, Adult: adult, ConfirmPayment: pay, ConfirmCountry: country}
		if err := store.SavePaymentChecks(r.Context(), id, checks); err != nil {
			writeGiftError(w, http.StatusInternalServerError, "500", "could not save payment checks")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "200"})
	}))
	mux.HandleFunc("POST /api/payment/large_order/apply", s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		if largeOrders == nil {
			writeGiftError(w, http.StatusServiceUnavailable, "503", "large order service unavailable")
			return
		}
		user, found, err := accounts.Find(r.Context(), id)
		if err != nil || !found {
			writeGiftError(w, http.StatusInternalServerError, "500", "could not read applicant")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > 128 || strings.ContainsAny(key, " \t") {
			writeGiftError(w, http.StatusBadRequest, "400", "invalid Idempotency-Key")
			return
		}
		if key == "" {
			key = rand.Text()
		}
		w.Header().Set("Idempotency-Key", key)
		w.Header().Set("Cache-Control", "no-store")
		created, complete, err := store.ReserveLargeOrder(r.Context(), id, key)
		if err != nil {
			writeGiftError(w, http.StatusInternalServerError, "500", "could not reserve large order")
			return
		}
		if complete {
			httpx.JSON(w, http.StatusOK, map[string]int{"orderId": 0})
			return
		}
		if !created {
			largeOrderPending(w)
			return
		}
		providerOrder, err := largeOrders.Create(r.Context(), id)
		if err != nil {
			largeOrderPending(w)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := store.RecordLargeOrder(ctx, id, key, providerOrder); err != nil {
			largeOrderPending(w)
			return
		}
		searchName := user.Account
		if user.Profile != nil {
			searchName = user.Profile.SearchName
		}
		if err := largeOrders.Notify(ctx, user.Email, user.Account, searchName); err != nil {
			largeOrderPending(w)
			return
		}
		if err := store.CompleteLargeOrder(ctx, id, key); err != nil {
			largeOrderPending(w)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]int{"orderId": 0})
	}))
	forward := s.authenticated(func(w http.ResponseWriter, r *http.Request, id int64) {
		response, err := proxy.Forward(r, id)
		if err != nil {
			switch {
			case errors.Is(err, payment.ErrProxyRoute):
				writeGiftError(w, http.StatusNotFound, "404", err.Error())
			case errors.Is(err, payment.ErrProxyInput):
				writeGiftError(w, http.StatusBadRequest, "400", err.Error())
			default:
				writeGiftError(w, http.StatusBadGateway, "502", "payment provider unavailable")
			}
			return
		}
		defer response.Body.Close()
		for _, header := range []string{"Content-Type", "Content-Disposition", "Location", "Retry-After"} {
			if value := response.Header.Get(header); value != "" {
				w.Header().Set(header, value)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	})
	mux.HandleFunc("GET /api/payment/{path...}", forward)
	mux.HandleFunc("POST /api/payment/{path...}", forward)
}

func paymentCheck(params url.Values, name string, required bool) (bool, bool) {
	values := params[name]
	if len(values) == 0 {
		return false, !required
	}
	if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		return false, false
	}
	return values[0] == "true", true
}

func largeOrderPending(w http.ResponseWriter) {
	writeGiftError(w, http.StatusConflict, "409", "large order requires reconciliation")
}
