package payment

import (
	"context"
	"sync"
	"time"
)

const CheckTTL = 3 * 365 * 24 * time.Hour

type Checks struct {
	Confirm        bool `json:"confirm_check"`
	Adult          bool `json:"adult_check"`
	ConfirmPayment bool `json:"confirm_pay_check"`
	ConfirmCountry bool `json:"confirm_country_check"`
}

type CheckStore interface {
	PaymentChecks(ctx context.Context, userID int64) (Checks, error)
	SavePaymentChecks(ctx context.Context, userID int64, checks Checks) error
}

type Store interface {
	CheckStore
	LargeOrderStore
}

type MemoryChecks struct {
	mu          sync.Mutex
	users       map[int64]savedChecks
	largeOrders map[largeOrderKey]memoryLargeOrder
}

type savedChecks struct {
	value   Checks
	expires time.Time
}

func (m *MemoryChecks) PaymentChecks(ctx context.Context, userID int64) (Checks, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Checks{}, err
	}
	saved := m.users[userID]
	if !time.Now().Before(saved.expires) {
		delete(m.users, userID)
		return Checks{}, nil
	}
	return saved.value, nil
}

func (m *MemoryChecks) SavePaymentChecks(ctx context.Context, userID int64, checks Checks) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.users == nil {
		m.users = make(map[int64]savedChecks)
	}
	m.users[userID] = savedChecks{value: checks, expires: time.Now().Add(CheckTTL)}
	return nil
}
