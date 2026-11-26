package posts

import (
	"context"

	"open_live_platform/internal/payment"
)

type OrderKind int

const (
	Unlock OrderKind = 1
	Tip    OrderKind = 2
)

type OrderState int

const (
	OrderPending OrderState = iota
	OrderPaid
	OrderDeclined
)

type Order struct {
	Number  string
	PostID  int64
	ActorID int64
	Kind    OrderKind
	Points  int64
	Key     string
	State   OrderState
	New     bool
}

// OrderStore persists a reservation before any debit leaves the platform.
type OrderStore interface {
	ReservePostOrder(context.Context, Order) (Order, error)
	CompletePostOrder(context.Context, Order, *payment.Receipt) error
}
