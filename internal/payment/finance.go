package payment

import (
	"context"
	"encoding/json"
	"net/http"
)

type FinanceCredit struct {
	UserID          int64 `json:"user_id"`
	Points          int64 `json:"point"`
	PointExpireDays int64 `json:"point_expire_days"`
	OrderID         int64 `json:"order_id"`
	BadDebtState    int64 `json:"bad_debt_state"`
}

type LargeOrderCredit struct {
	UserID          int64  `json:"user_id"`
	Number          string `json:"no"`
	TransactionID   string `json:"transaction_id"`
	PayAmount       string `json:"pay_amount"`
	Points          int64  `json:"point"`
	PointExpireDays int64  `json:"point_expire_days"`
	Remark          string `json:"remark"`
	OperatorID      int64  `json:"create_by"`
}

type financeConfirmation json.RawMessage

func (c *Client) FinanceCredit(ctx context.Context, request FinanceCredit) (json.RawMessage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var response financeConfirmation
	err = c.request(ctx, http.MethodPost, "/api/payment/point/add", request.UserID, body, &response)
	return json.RawMessage(response), err
}

func (c *Client) CreditLargeOrder(ctx context.Context, request LargeOrderCredit) (json.RawMessage, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var response financeConfirmation
	err = c.request(ctx, http.MethodPost, "/api/payment/large_order/update", request.UserID, body, &response)
	return json.RawMessage(response), err
}

func (c *Client) FlagBadDebt(ctx context.Context, userID, orderID int64) ([]Receipt, error) {
	body, err := json.Marshal(struct {
		ID int64 `json:"id"`
	}{orderID})
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []Receipt `json:"items"`
	}
	if err := c.request(ctx, http.MethodPost, "/api/payment/order/set_bad_debt", userID, body, &result); err != nil {
		return nil, err
	}
	if result.Items == nil {
		return nil, ErrUnavailable
	}
	ids, numbers := map[int64]struct{}{}, map[string]struct{}{}
	for _, item := range result.Items {
		if item.OrderID <= 0 || item.OrderNo == "" || item.TotalPoints <= 0 || item.BadDebt < 0 || item.BadDebt > item.TotalPoints {
			return nil, ErrUnavailable
		}
		if _, exists := ids[item.OrderID]; exists {
			return nil, ErrUnavailable
		}
		if _, exists := numbers[item.OrderNo]; exists {
			return nil, ErrUnavailable
		}
		ids[item.OrderID], numbers[item.OrderNo] = struct{}{}, struct{}{}
	}
	return result.Items, nil
}
