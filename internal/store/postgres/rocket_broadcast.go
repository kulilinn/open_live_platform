package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/subscription"
)

func (s *Store) sendRocketComboBroadcast(ctx context.Context, conn *pgx.Conn, signal roomSignal) error {
	receiver := s.relayDeviceReceiver.Load()
	if receiver == nil {
		return nil
	}
	message := relay.RustDeviceSignal{Scope: signal.RelayScope, Kind: "global"}
	if !receiver.accept(message) {
		return nil
	}
	var notice json.RawMessage
	var combo int64
	err := conn.QueryRow(ctx, `SELECT config.value,f.combo_at_settlement FROM rocket_refuel_orders f
		JOIN gift_orders o ON o.id=f.order_id AND o.state=1 AND o.room_id=$2
		JOIN public_catalog p ON p.section='broadcast_configs'
		CROSS JOIN LATERAL (SELECT v.value FROM jsonb_array_elements(COALESCE(NULLIF(f.plan->'Notices','null'::jsonb),'[]'::jsonb)) WITH ORDINALITY v(value,ranking)
			WHERE v.value->>'notice_type'='2' AND COALESCE(v.value->>'del_flag','0')='0'
			AND v.value->>'config_condition'=f.combo_at_settlement::text ORDER BY v.ranking LIMIT 1) selected
		CROSS JOIN LATERAL jsonb_array_elements(p.records) config(value)
		WHERE f.order_id=$1 AND selected.value->>'horse_race_lamp_flag'='0'
		AND selected.value->>'broadcast_config_id'=config.value->>'id' AND config.value->>'id'<>'0'`, signal.MessageID, signal.RoomID).Scan(&notice, &combo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(notice, &config); err != nil {
		return err
	}
	order, err := scanGiftOrder(conn.QueryRow(ctx, `SELECT `+giftOrderColumns+` FROM gift_orders WHERE id=$1 AND state=1`, signal.MessageID))
	if errors.Is(err, gift.ErrOrderNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	points, _, err := subscriptionExperience(ctx, conn, order.Sender.ID, 0)
	if err != nil {
		return err
	}
	senderLevel, _ := subscription.ExperienceLevels(points, 0)
	points, _, err = subscriptionExperience(ctx, conn, order.Receiver.ID, 0)
	if err != nil {
		return err
	}
	receiverLevel, _ := subscription.ExperienceLevels(points, 0)
	data := map[string]any{
		"gift_id": order.Item.ID, "gift_name": order.Item.Name, "gift_icon": order.Item.Icon, "combo": combo,
		"sender_id": order.Sender.ID, "sender_name": order.Sender.Name, "sender_platform_level": senderLevel, "sender_photo": order.Sender.Photo,
		"receiver_id": order.Receiver.ID, "receiver_name": order.Receiver.Name, "receiver_platform_level": receiverLevel, "receiver_photo": order.Receiver.Photo,
		"receiver_search_name": order.Receiver.SearchName, "date_room_id": order.Input.Room,
	}
	for _, key := range []string{"jump_type", "jump_target", "jump_background", "jump_name", "background"} {
		data[key] = config[key]
	}
	message.Data, err = json.Marshal(map[string]any{"broadcast_type": 6, "data": data})
	if err == nil {
		receiver.receive(message)
	}
	return err
}

func (s *Store) ConfigureRocketBroadcastScope(appKey string) {
	s.rocketRelayScope = appKey
}
