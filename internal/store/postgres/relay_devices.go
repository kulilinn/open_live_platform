package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/relay"
)

const relayDevicePrefix = "relay_device:"

type relayDeviceReceiver struct {
	accept  func(relay.RustDeviceSignal) bool
	receive func(relay.RustDeviceSignal)
}

func (s *Store) PublishRustDeviceSignal(ctx context.Context, signal relay.RustDeviceSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	if len(payload)+len(relayDevicePrefix) < 8000 {
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1,$2)`, s.eventChannel, relayDevicePrefix+string(payload))
		return err
	}
	data := signal.Data
	signal.Data = nil
	meta, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `WITH stored AS (
		INSERT INTO relay_device_payloads(app_key,node,user_id,kind,payload,expires_at)
		VALUES($4,$5,$6,$7,$8::jsonb,statement_timestamp()+INTERVAL '1 minute') RETURNING id
	), expired AS (
		DELETE FROM relay_device_payloads WHERE id IN (
			SELECT id FROM relay_device_payloads WHERE expires_at<=statement_timestamp()
			ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)
	)
	SELECT pg_notify($1,$2||jsonb_set($3::jsonb,'{ref}',to_jsonb(stored.id))::text) FROM stored`,
		s.eventChannel, relayDevicePrefix, string(meta), signal.Scope, signal.Node, signal.UserID, signal.Kind, string(data))
	return err
}

func (s *Store) ReceiveRustDeviceSignals(accept func(relay.RustDeviceSignal) bool, receive func(relay.RustDeviceSignal)) {
	s.relayDeviceReceiver.Store(&relayDeviceReceiver{accept: accept, receive: receive})
}

func (s *Store) readRelayDeviceSignal(ctx context.Context, conn *pgx.Conn, payload string) (bool, error) {
	body, ok := strings.CutPrefix(payload, relayDevicePrefix)
	if !ok {
		return false, nil
	}
	if receive := s.relayDeviceReceiver.Load(); receive != nil {
		var signal relay.RustDeviceSignal
		if json.Unmarshal([]byte(body), &signal) != nil || !receive.accept(signal) {
			return true, nil
		}
		if signal.Ref != 0 {
			err := conn.QueryRow(ctx, `SELECT payload FROM relay_device_payloads
				WHERE id=$1 AND app_key=$2 AND node=$3 AND user_id=$4 AND kind=$5 AND expires_at>statement_timestamp()`,
				signal.Ref, signal.Scope, signal.Node, signal.UserID, signal.Kind).Scan(&signal.Data)
			if errors.Is(err, pgx.ErrNoRows) {
				return true, nil
			}
			if err != nil {
				return true, err
			}
		}
		receive.receive(signal)
	}
	return true, nil
}
