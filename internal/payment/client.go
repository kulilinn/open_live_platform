package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInsufficientPoints = errors.New("insufficient points")
	ErrUnavailable        = errors.New("payment provider unavailable")
)

type Receipt struct {
	OrderID     int64  `json:"order_id,string"`
	OrderNo     string `json:"order_no"`
	BadDebt     int64  `json:"total_bad_debt_point"`
	TotalPoints int64  `json:"total_point"`
}

type Client struct {
	host, appID string
	client      *http.Client
}

func NewClient(host, appID string) (*Client, error) {
	if host == "" && appID == "" {
		return nil, nil
	}
	parsed, err := url.Parse(host)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSpace(appID) == "" {
		return nil, errors.New("invalid PAYMENT_HOST or PAYMENT_APPID")
	}
	return &Client{host: strings.TrimRight(host, "/"), appID: appID, client: &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Balance(ctx context.Context, userID int64) (int64, error) {
	var data struct{ Point *int64 }
	if err := c.request(ctx, http.MethodGet, "/api/payment/user/point", userID, nil, &data); err != nil {
		return 0, err
	}
	if data.Point == nil || *data.Point < 0 {
		return 0, ErrUnavailable
	}
	return *data.Point, nil
}

func (c *Client) Decrease(ctx context.Context, userID, points int64, orderNo string) (Receipt, error) {
	if points <= 0 || orderNo == "" {
		return Receipt{}, errors.New("invalid point debit")
	}
	body, err := json.Marshal(struct {
		UserID  int64  `json:"user_id"`
		Points  int64  `json:"point"`
		OrderNo string `json:"order_no"`
	}{userID, points, orderNo})
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	if err := c.request(ctx, http.MethodPost, "/api/payment/point/decrease", userID, body, &receipt); err != nil {
		return Receipt{}, err
	}
	if receipt.OrderID <= 0 || receipt.OrderNo == "" || receipt.TotalPoints != points || receipt.BadDebt < 0 || receipt.BadDebt > points {
		return Receipt{}, ErrUnavailable
	}
	return receipt, nil
}

func (c *Client) request(ctx context.Context, method, path string, userID int64, body []byte, data any) error {
	if c == nil {
		return ErrUnavailable
	}
	var payloadReader io.Reader
	if body != nil {
		payloadReader = io.NopCloser(bytes.NewReader(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.host+path, payloadReader)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("APPID", c.appID)
	req.Header.Set("USER_ID", strconv.FormatInt(userID, 10))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	}
	res, err := c.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer res.Body.Close()
	const maxSize = 64 * 1024
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxSize+1))
	if err != nil || len(payload) > maxSize {
		return ErrUnavailable
	}
	var result struct {
		Code   int
		Reason string
		Data   json.RawMessage
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return ErrUnavailable
	}
	if res.StatusCode == http.StatusOK && result.Code == 200 {
		if raw, ok := data.(*financeConfirmation); ok {
			*raw = financeConfirmation(payload)
			return nil
		}
		if len(result.Data) == 0 || string(result.Data) == "null" || json.Unmarshal(result.Data, data) != nil {
			return ErrUnavailable
		}
		return nil
	}
	if result.Reason == "POINT_NOT_ENOUGH" && res.StatusCode < 500 {
		return ErrInsufficientPoints
	}
	return ErrUnavailable
}
