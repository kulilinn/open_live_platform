package httpapi

import (
	"net/http"
	"net/netip"
	"open_live_platform/internal/geo"
	"open_live_platform/internal/transport/httpx"
	"slices"
	"strings"
)

func RegisterGeo(mux *http.ServeMux, c geo.Config, lookup func(netip.Addr) string) {
	country := func(r *http.Request) string {
		return lookup(httpx.ClientIP(r, c.TrustedProxies))
	}
	filter := strings.FieldsFunc(c.Filter, func(r rune) bool { return r == ';' || r == ',' })
	mux.HandleFunc("GET /ipchecker", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]any{"status": "200", "blackList": slices.Contains(filter, country(r)), "xpt": c.XPT})
	})
	mux.HandleFunc("GET /ipchecker_v1", func(w http.ResponseWriter, r *http.Request) {
		status := "200"
		if v := country(r); v == "JP" || v == "CN" {
			status = "403"
		}
		httpx.JSON(w, 200, map[string]string{"status": status})
	})
}
