package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"open_live_platform/internal/notifyemail"
)

type LargeOrderConfig struct {
	Region, NotificationURL, FromAddress, FromName string
	TemplateCN, TemplateTW, TemplateJP, TemplateEN string
	ConfirmationTemplate                           string
}

type LargeOrderService struct {
	billing *Client
	region  string
	orderNotification
}

type orderNotification struct {
	template string
	client   *notifyemail.Client
}

type LargeOrderConfirmation struct{ orderNotification }

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
	notification, err := newOrderNotification(config, template)
	if err != nil || billing == nil {
		return nil, errors.New("large-order configuration requires payment, notification, sender, and the region's template")
	}
	return &LargeOrderService{billing: billing, region: region, orderNotification: notification}, nil
}

func NewLargeOrderConfirmation(config LargeOrderConfig) (*LargeOrderConfirmation, error) {
	if config.ConfirmationTemplate == "" {
		return nil, nil
	}
	notification, err := newOrderNotification(config, config.ConfirmationTemplate)
	if err != nil {
		return nil, err
	}
	return &LargeOrderConfirmation{notification}, nil
}

func newOrderNotification(config LargeOrderConfig, template string) (orderNotification, error) {
	client, err := notifyemail.New(notifyemail.Config{URL: config.NotificationURL, From: notifyemail.Address{Address: config.FromAddress, Name: config.FromName}})
	if err != nil || template == "" {
		return orderNotification{}, errors.New("invalid large-order notification configuration")
	}
	return orderNotification{template: template, client: client}, nil
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

func (s *LargeOrderService) Notify(ctx context.Context, email, displayName, searchName string) error {
	return s.send(ctx, email, displayName, map[string]string{"nick_name": displayName, "search_name": searchName, "email": email})
}

func (s *LargeOrderConfirmation) Notify(ctx context.Context, email, displayName, searchName, order string) error {
	return s.send(ctx, email, displayName, map[string]string{"nick_name": displayName, "search_name": searchName, "description": order})
}

func (s orderNotification) send(ctx context.Context, email, displayName string, fields map[string]string) error {
	return s.client.Send(ctx, s.template, notifyemail.Address{Address: email, Name: displayName}, fields, "")
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
