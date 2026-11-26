package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, _ := netip.ParseAddr(host)
	peer = peer.Unmap()
	isProxy := func(addr netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	if !isProxy(peer) {
		return peer
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) == 1 && chain[0] == "" {
		chain[0] = r.Header.Get("X-Real-IP")
	}
	for i := len(chain) - 1; i >= 0 && isProxy(peer); i-- {
		next, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			break
		}
		peer = next.Unmap()
	}
	return peer
}

type BrowserConfig struct {
	Origins        []string
	RateLimit      bool
	TrustedProxies []netip.Prefix
}

const corsMethods = "GET, POST, PATCH, DELETE, OPTIONS"
const corsHeaders = "Authorization, Accept, Content-Type, Upgrade, Host, Connection, Origin, Sec-WebSocket-Key, Sec-WebSocket-Protocol, X-Loverose-Chl, X-Loverose-R18, X-Loverose-Did, X-Loverose-Lang, X-Lr-Stt-Lang, Agora-Signature, X-Forwarded-For, X-Forwarded-Host, X-Forwarded-Proto, Forwarded, X-Real-IP, X-Loverose-Anonymous-Id, Is-Innow, X-Loverose-Version, X-Loverose-Auth-Version, X-User-Tag, X-Loverose-Uid, Idempotency-Key"

func BrowserHandler(mux *http.ServeMux, c BrowserConfig) http.Handler {
	if len(c.Origins) == 0 && !c.RateLimit {
		return mux
	}
	var limiter ipLimiter
	wildcard := slices.Contains(c.Origins, "*")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if len(c.Origins) > 0 && origin != "" {
			allowed := wildcard || slices.Contains(c.Origins, origin)
			w.Header().Add("Vary", "Origin")
			if allowed {
				value := origin
				if wildcard {
					value = "*"
				} else {
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				w.Header().Set("Access-Control-Allow-Origin", value)
			}
			if r.Method == "OPTIONS" && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				if allowed {
					w.Header().Set("Access-Control-Allow-Methods", corsMethods)
					w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if !allowed && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		if c.RateLimit {
			_, route := mux.Handler(r)
			if limitedRoute(route) && !limiter.allow(ipPath{ClientIP(r, c.TrustedProxies), r.URL.Path}, time.Now()) {
				w.Header().Set("Retry-After", "6")
				http.Error(w, "Too many requests! Please wait for one minute.", http.StatusTooManyRequests)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func limitedRoute(route string) bool {
	switch route {
	case "GET /ipchecker", "GET /ipchecker_v1", "POST /api/auth/signup", "POST /api/auth/signin", "POST /api/auth/exchange_token", "GET /api/auth/code", "POST /api/auth/code", "GET /api/pwa/anchor", "POST /api/pwa/anchor", "POST /api/connector/acquire", "POST /api/connector/vibrate", "POST /api/connector/remote", "POST /api/chatbot/{botid}", "GET /api/chatbot/{botid}/{token}", "POST /api/feedback/submit":
		return true
	}
	return false
}

type ipPath struct {
	ip   netip.Addr
	path string
}
type visits struct {
	count   int
	expires time.Time
}
type ipLimiter struct {
	mu      sync.Mutex
	entries map[ipPath]visits
	sweep   time.Time
}

func (l *ipLimiter) allow(key ipPath, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.sweep.After(now) {
		if l.entries == nil {
			l.entries = make(map[ipPath]visits)
		}
		for k, v := range l.entries {
			if !v.expires.After(now) {
				delete(l.entries, k)
			}
		}
		l.sweep = now.Add(6 * time.Second)
	}
	v := l.entries[key]
	if !v.expires.After(now) {
		v.count = 0
	}
	if v.count > 600 {
		return false
	}
	l.entries[key] = visits{v.count + 1, now.Add(6 * time.Second)}
	return true
}
