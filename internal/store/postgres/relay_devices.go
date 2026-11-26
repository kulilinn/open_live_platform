package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"open_live_platform/internal/relay"
)

const relayDevicePrefix = "relay_device:"

func (s *Store) PublishRustDeviceSignal(ctx context.Context, signal relay.RustDeviceSignal) error {
	payload, err := json.Marshal(signal)
	if err != nil {
		return err
	}
	if len(payload)+len(relayDevicePrefix) >= 8000 {
		return errors.New("relay device notification exceeds PostgreSQL payload limit")
	}
	_, err = s.pool.Exec(ctx, `SELECT pg_notify($1,$2)`, s.eventChannel, relayDevicePrefix+string(payload))
	return err
}

func (s *Store) ReceiveRustDeviceSignals(receive func(relay.RustDeviceSignal)) {
	s.relayDeviceReceiver.Store(&receive)
}

func (s *Store) readRelayDeviceSignal(payload string) bool {
	body, ok := strings.CutPrefix(payload, relayDevicePrefix)
	if !ok {
		return false
	}
	if receive := s.relayDeviceReceiver.Load(); receive != nil {
		var signal relay.RustDeviceSignal
		if json.Unmarshal([]byte(body), &signal) == nil {
			(*receive)(signal)
		}
	}
	return true
}
