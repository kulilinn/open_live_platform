package payment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

var (
	ErrProxyRoute = errors.New("payment route is not configured")
	ErrProxyInput = errors.New("invalid payment request")
)

type Proxy struct {
	billing     *Client
	borrowAppID string
	routes      map[string]struct{}
}

func NewProxy(billing *Client, borrowAppID, routes string) (Proxy, error) {
	proxy := Proxy{billing: billing, borrowAppID: borrowAppID, routes: make(map[string]struct{})}
	for _, entry := range strings.Split(routes, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		method, endpoint, _ := strings.Cut(entry, " ")
		if (method != http.MethodGet && method != http.MethodPost) || !strings.HasPrefix(endpoint, "/api/payment/") ||
			strings.ContainsAny(endpoint, " %{}?&#\\\r\n\t") || path.Clean(endpoint) != endpoint || protectedPaymentPath(endpoint) {
			return Proxy{}, fmt.Errorf("invalid or reserved payment proxy route %q", entry)
		}
		proxy.routes[entry] = struct{}{}
	}
	return proxy, nil
}

func protectedPaymentPath(endpoint string) bool {
	switch endpoint {
	case "/api/payment/point/decrease", "/api/payment/point/list", "/api/payment/large_order/create",
		"/api/payment/large_order/update", "/api/payment/order/set_bad_debt", "/api/payment/check/edit",
		"/api/payment/check/get", "/api/payment/large_order/apply", "/api/payment/points/get":
		return true
	default:
		return false
	}
}

func (p Proxy) Forward(inbound *http.Request, userID int64) (*http.Response, error) {
	endpoint := inbound.URL.Path
	if _, allowed := p.routes[inbound.Method+" "+endpoint]; !allowed || inbound.URL.EscapedPath() != endpoint {
		return nil, ErrProxyRoute
	}
	if p.billing == nil {
		return nil, ErrUnavailable
	}
	uid := strconv.FormatInt(userID, 10)
	params, err := url.ParseQuery(inbound.URL.RawQuery)
	if err != nil || claimedOtherUser(params, uid) {
		return nil, ErrProxyInput
	}
	upstream := p.billing.host + endpoint
	if inbound.URL.RawQuery != "" {
		upstream += "?" + inbound.URL.RawQuery
	}
	var body io.Reader
	var bodySize int64
	if inbound.Method == http.MethodPost {
		const maxBody = 1 << 20
		data, err := io.ReadAll(io.LimitReader(inbound.Body, maxBody+1))
		if err != nil || len(data) > maxBody || !paymentBodyMatchesUser(data, inbound.Header.Get("Content-Type"), uid) {
			return nil, ErrProxyInput
		}
		// A streamed body prevents the transport from replaying a write.
		body, bodySize = io.NopCloser(bytes.NewReader(data)), int64(len(data))
	}
	request, err := http.NewRequestWithContext(inbound.Context(), inbound.Method, upstream, body)
	if err != nil {
		return nil, ErrProxyInput
	}
	request.ContentLength = bodySize
	for _, header := range []string{"Accept", "Content-Type", "Idempotency-Key", "X-Loverose-Lang"} {
		if value := inbound.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	request.Header.Set("APPID", p.billing.appID)
	request.Header.Set("USER_ID", uid)
	request.Header.Set("BORROW_APPID", p.borrowAppID)
	response, err := p.billing.client.Do(request)
	if err != nil {
		return nil, ErrUnavailable
	}
	return response, nil
}

func claimedOtherUser(params url.Values, uid string) bool {
	for _, key := range []string{"user_id", "userId", "uid"} {
		for _, claimed := range params[key] {
			if claimed != uid {
				return true
			}
		}
	}
	return false
}

func paymentBodyMatchesUser(data []byte, contentType, uid string) bool {
	if len(data) == 0 {
		return true
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch mediaType {
	case "application/json":
		// Read every top-level identity field, including duplicates, before
		// forwarding the provider's original JSON without numeric rounding.
		decoder := json.NewDecoder(bytes.NewReader(data))
		start, err := decoder.Token()
		if err != nil || start != json.Delim('{') {
			return false
		}
		for decoder.More() {
			key, err := decoder.Token()
			var value json.RawMessage
			if err != nil || decoder.Decode(&value) != nil {
				return false
			}
			if key == "user_id" || key == "userId" || key == "uid" {
				var claimed string
				if string(value) != uid && (json.Unmarshal(value, &claimed) != nil || claimed != uid) {
					return false
				}
			}
		}
		_, err = decoder.Token()
		return err == nil && decoder.Decode(new(json.RawMessage)) == io.EOF
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(data))
		return err == nil && !claimedOtherUser(values, uid)
	default:
		return false
	}
}
