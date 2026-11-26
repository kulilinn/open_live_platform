package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

type LargeOrderConfig struct {
	Region, NotificationURL, FromAddress, FromName string
	TemplateCN, TemplateTW, TemplateJP, TemplateEN string
}

type LargeOrderService struct {
	billing                        *Client
	region, notification, template string
	from                           orderEmailAddress
	client                         *http.Client
}

type LargeOrderStore interface {
	ReserveLargeOrder(ctx context.Context, userID int64, key string) (newAttempt, complete bool, err error)
	RecordLargeOrder(ctx context.Context, userID int64, key, providerOrder string) error
	CompleteLargeOrder(ctx context.Context, userID int64, key string) error
}

type memoryLargeOrder struct {
	providerOrder string
	complete      bool
}

func NewLargeOrderService(billing *Client, config LargeOrderConfig) (*LargeOrderService, error) {
	if config.Region == "" {
		return nil, nil
	}
	region := strings.ToUpper(config.Region)
	if len(region) != 2 || region[0] < 'A' || region[0] > 'Z' || region[1] < 'A' || region[1] > 'Z' {
		return nil, errors.New("PAYMENT_LARGE_ORDER_REGION must be a two-letter region")
	}
	template := config.TemplateEN
	switch region {
	case "CN":
		template = config.TemplateCN
	case "HK", "TW":
		template = config.TemplateTW
	case "JP":
		template = config.TemplateJP
	}
	parsed, err := url.Parse(config.NotificationURL)
	address, mailErr := mail.ParseAddress(config.FromAddress)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		mailErr != nil || template == "" || billing == nil {
		return nil, errors.New("large-order configuration requires payment, notification, sender, and the region's template")
	}
	return &LargeOrderService{billing: billing, region: region, notification: config.NotificationURL, template: template,
		from: orderEmailAddress{Address: address.Address, Name: config.FromName}, client: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}, nil
}

func (s *LargeOrderService) Create(ctx context.Context, userID int64) (string, error) {
	body, err := json.Marshal(map[string]any{"user_id": userID, "region": s.region, "point": 0, "point_expire_days": 0})
	if err != nil {
		return "", err
	}
	var order struct{ No string }
	if err := s.billing.request(ctx, http.MethodPost, "/api/payment/large_order/create", userID, body, &order); err != nil || order.No == "" {
		return "", ErrUnavailable
	}
	return order.No, nil
}

type orderEmailAddress struct {
	Address string `json:"address"`
	Name    string `json:"name"`
}

func (s *LargeOrderService) Notify(ctx context.Context, email, displayName, searchName string) error {
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return errors.New("invalid large-order receipt email")
	}
	inner, err := json.Marshal(map[string]any{
		"template_key": s.template, "track_clicks": true, "track_opens": true, "from": s.from,
		"to":         []map[string]orderEmailAddress{{"email_address": {Address: email, Name: displayName}}},
		"merge_info": map[string]string{"nick_name": displayName, "search_name": searchName, "email": email},
	})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"type": 3, "content": string(inner)})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.notification, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("large order notification was not accepted")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}

type largeOrderKey struct {
	userID int64
	key    string
}

func (m *MemoryChecks) ReserveLargeOrder(ctx context.Context, userID int64, key string) (bool, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	lookup := largeOrderKey{userID, key}
	if state, exists := m.largeOrders[lookup]; exists {
		return false, state.complete, nil
	}
	if m.largeOrders == nil {
		m.largeOrders = make(map[largeOrderKey]memoryLargeOrder)
	}
	m.largeOrders[lookup] = memoryLargeOrder{}
	return true, false, nil
}

func (m *MemoryChecks) RecordLargeOrder(ctx context.Context, userID int64, key, providerOrder string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	lookup := largeOrderKey{userID, key}
	state, exists := m.largeOrders[lookup]
	if !exists || state.providerOrder != "" {
		return errors.New("large order is not pending")
	}
	state.providerOrder = providerOrder
	m.largeOrders[lookup] = state
	return nil
}

func (m *MemoryChecks) CompleteLargeOrder(ctx context.Context, userID int64, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	lookup := largeOrderKey{userID, key}
	state, exists := m.largeOrders[lookup]
	if !exists || state.providerOrder == "" || state.complete {
		return errors.New("large order is not recorded")
	}
	state.complete = true
	m.largeOrders[lookup] = state
	return nil
}
