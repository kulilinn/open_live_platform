package httpapi

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"unicode"

	"open_live_platform/internal/transport/httpx"
)

type accessSettings interface {
	AccessSetting(context.Context, string) (string, error)
}

type staffSettingContext struct{}

type accessHandler struct {
	*http.ServeMux
	inner   http.Handler
	store   accessSettings
	proxies []netip.Prefix
}

func WithAccessSettings(mux *http.ServeMux, inner http.Handler, store accessSettings, proxies []netip.Prefix) http.Handler {
	return &accessHandler{mux, inner, store, proxies}
}

func (h *accessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		content, err := h.store.AccessSetting(r.Context(), "black_ip")
		if err != nil {
			httpx.Error(w, 503, "could not read access settings")
			return
		}
		ip := httpx.ClientIP(r, h.proxies)
		if ip.IsValid() && hasSettingEntry(content, ip.String()) {
			httpx.Error(w, 401, "invalid bearer token")
			return
		}
	}
	switch path {
	case "/api/auth/signin", "/api/auth/verifygoogle", "/api/auth/exchange_token":
		content, err := h.store.AccessSetting(r.Context(), "white_user")
		if err != nil {
			httpx.Error(w, 503, "could not read access settings")
			return
		}
		if content != "" {
			r = r.WithContext(context.WithValue(r.Context(), staffSettingContext{}, content))
		}
	}
	h.inner.ServeHTTP(w, r)
}

// A setting stores a delimited list (or a JSON array), shared with the original operator UI.
func hasSettingEntry(content, entry string) bool {
	if content == "" || entry == "" {
		return false
	}
	entries := strings.FieldsFunc(content, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(",;|，；[]\"", r) })
	return slices.ContainsFunc(entries, func(s string) bool { return strings.EqualFold(s, entry) })
}
