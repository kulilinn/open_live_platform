package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/payment"
)

var _ payment.CheckStore = (*Store)(nil)
var _ payment.LargeOrderStore = (*Store)(nil)

func (s *Store) PaymentChecks(ctx context.Context, userID int64) (payment.Checks, error) {
	var checks payment.Checks
	err := s.pool.QueryRow(ctx, `SELECT confirm_check,adult_check,confirm_pay_check,confirm_country_check
		FROM payment_checks WHERE user_id=$1 AND expires_at>statement_timestamp()`, userID).
		Scan(&checks.Confirm, &checks.Adult, &checks.ConfirmPayment, &checks.ConfirmCountry)
	if errors.Is(err, pgx.ErrNoRows) {
		return payment.Checks{}, nil
	}
	return checks, err
}

func (s *Store) SavePaymentChecks(ctx context.Context, userID int64, checks payment.Checks) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO payment_checks (user_id,confirm_check,adult_check,confirm_pay_check,confirm_country_check,expires_at)
		VALUES($1,$2,$3,$4,$5,statement_timestamp()+make_interval(secs=>$6)) ON CONFLICT(user_id) DO UPDATE SET
		confirm_check=EXCLUDED.confirm_check,adult_check=EXCLUDED.adult_check,confirm_pay_check=EXCLUDED.confirm_pay_check,
		confirm_country_check=EXCLUDED.confirm_country_check,expires_at=EXCLUDED.expires_at`,
		userID, checks.Confirm, checks.Adult, checks.ConfirmPayment, checks.ConfirmCountry, payment.CheckTTL.Seconds())
	return err
}

func (s *Store) ReserveLargeOrder(ctx context.Context, userID int64, key string) (bool, bool, error) {
	result, err := s.pool.Exec(ctx, `INSERT INTO payment_large_order_attempts(user_id,request_key)
		VALUES($1,$2) ON CONFLICT(user_id,request_key) DO NOTHING`, userID, key)
	if err != nil {
		return false, false, err
	}
	if result.RowsAffected() != 0 {
		return true, false, nil
	}
	var complete bool
	err = s.pool.QueryRow(ctx, `SELECT notified_at IS NOT NULL FROM payment_large_order_attempts WHERE user_id=$1 AND request_key=$2`, userID, key).
		Scan(&complete)
	return false, complete, err
}

func (s *Store) RecordLargeOrder(ctx context.Context, userID int64, key, providerOrder string) error {
	result, err := s.pool.Exec(ctx, `UPDATE payment_large_order_attempts SET provider_order=$3
		WHERE user_id=$1 AND request_key=$2 AND provider_order IS NULL`, userID, key, providerOrder)
	if err == nil && result.RowsAffected() == 0 {
		return errors.New("large order is not pending")
	}
	return err
}

func (s *Store) CompleteLargeOrder(ctx context.Context, userID int64, key string) error {
	result, err := s.pool.Exec(ctx, `UPDATE payment_large_order_attempts SET notified_at=statement_timestamp()
		WHERE user_id=$1 AND request_key=$2 AND provider_order IS NOT NULL AND notified_at IS NULL`, userID, key)
	if err == nil && result.RowsAffected() == 0 {
		return errors.New("large order is not recorded")
	}
	return err
}
