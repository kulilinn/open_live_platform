package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/live"
	"open_live_platform/internal/transport/httpx"
)

type roomGiftEffectsAPI struct {
	server
	store       gift.RoomEffectsStore
	coefficient float64
}

func RegisterRoomGiftEffects(mux *http.ServeMux, accounts auth.AccountStore, rooms live.RoomStore, speed float64) {
	store, ok := rooms.(gift.RoomEffectsStore)
	if !ok {
		return
	}
	a := roomGiftEffectsAPI{server{accounts: accounts, rooms: rooms}, store, speed}
	mux.HandleFunc("POST /api/date/gift/hardware/set", a.authenticated(a.setHardware))
	mux.HandleFunc("GET /api/date/gift/hardware/get/{room_id}", a.authenticated(a.hardwareSetting))
	mux.HandleFunc("GET /api/date/hardware/queue/get/{room_id}", a.authenticated(a.hardwareQueue))
	mux.HandleFunc("GET /api/date/rocket/state/get/{room_id}", a.authenticated(a.rocket("state")))
	mux.HandleFunc("GET /api/1vN/rocket/refuel/rank/{room}", a.authenticated(a.rocket("rank")))
	mux.HandleFunc("GET /api/date/get/rocket/final/result", a.authenticated(a.rocket("final")))
	mux.HandleFunc("POST /api/date/rocket/change/speed", a.authenticated(a.speed))
	mux.HandleFunc("POST /api/date/rocket/pop_slap_face", a.authenticated(a.slap))
	mux.HandleFunc("POST /api/date/client/end/gift", a.authenticated(a.end))
}

func (a roomGiftEffectsAPI) setHardware(w http.ResponseWriter, r *http.Request, uid int64) {
	var input struct {
		Room  int64   `json:"dateRoomId,string"`
		Ratio float64 `json:"reduceTimeRatio"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.Room <= 0 || math.IsNaN(input.Ratio) || input.Ratio < 0 || input.Ratio > 100 {
		httpx.Error(w, 400, "invalid hardware duration setting")
		return
	}
	if err := a.store.ReduceGiftDuration(r.Context(), input.Room, uid, input.Ratio); err != nil {
		httpx.LiveError(w, err)
		return
	}
	dateGiftResult(w, "success", nil)
}

func (a roomGiftEffectsAPI) hardwareSetting(w http.ResponseWriter, r *http.Request, _ int64) {
	room, ok := httpx.PathID(w, r, "room_id")
	if !ok {
		return
	}
	ratio, err := a.store.GiftDurationReduction(r.Context(), room)
	dateGiftResult(w, ratio, err)
}

func (a roomGiftEffectsAPI) hardwareQueue(w http.ResponseWriter, r *http.Request, _ int64) {
	id, ok := httpx.PathID(w, r, "room_id")
	if !ok {
		return
	}
	room, err := a.rooms.Info(r.Context(), id)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	queue := make([]map[string]any, 0)
	var next any
	var remain, send int64
	if room.Online {
		effects, err := a.store.RoomHardware(r.Context(), room.ID, room.Session.LiveID)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		for _, effect := range effects {
			queue = append(queue, effect.HardwareView())
		}
		if len(effects) > 0 {
			next, send = queue[0], effects[0].Start.Unix()
			remain = max(0, effects[0].End.Unix()-time.Now().Unix())
		}
	}
	dateGiftResult(w, map[string]any{"next_start_gift": next, "hardware_queue": queue, "send_time": send, "remain_duration": remain}, nil)
}

func (a roomGiftEffectsAPI) rocket(kind string) userHandler {
	return func(w http.ResponseWriter, r *http.Request, _ int64) {
		roomText := r.PathValue("room_id")
		var wantedAnchor int64
		switch kind {
		case "final":
			roomText = r.URL.Query().Get("dateRoomId")
			wantedAnchor, _ = strconv.ParseInt(r.URL.Query().Get("anchorId"), 10, 64)
		case "rank":
			roomText = r.PathValue("room")
		}
		id, err := strconv.ParseInt(roomText, 10, 64)
		if err != nil || id < 1 {
			httpx.Error(w, 400, "invalid rocket room")
			return
		}
		room, err := a.rooms.Info(r.Context(), id)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		if kind == "final" && room.OwnerID != wantedAnchor {
			httpx.LiveError(w, live.ErrForbidden)
			return
		}
		effect, err := a.store.RoomRocket(r.Context(), id, room.Session.LiveID)
		if err != nil {
			httpx.LiveError(w, err)
			return
		}
		now := time.Now()
		active := room.Online && effect != nil && effect.Active(now)
		var rank []gift.RocketContribution
		if effect != nil && (active || kind == "rank") {
			rank, err = a.store.RocketRank(r.Context(), effect.Order.ID)
			if err != nil {
				httpx.LiveError(w, err)
				return
			}
		}
		if rank == nil {
			rank = []gift.RocketContribution{}
		}
		var result any
		switch kind {
		case "rank":
			result = rank
		case "final":
			var final json.RawMessage
			if effect != nil && !active {
				final = effect.FinalResult
			}
			result = map[string]any{"rocket_is_end": !active, "rocket_final_result": final}
		case "state":
			if active {
				encoded, _ := json.Marshal(rank)
				result = map[string]any{
					"departure_time": effect.Start, "flight_duration": int64(effect.End.Sub(effect.Start) / time.Second),
					"flight_remaining_time": max(0, effect.End.Unix()-now.Unix()), "vibration_intensity": effect.Speed,
					"contribute_rank": string(encoded), "sender_name": effect.Order.Sender.Name, "sender_photo": effect.Order.Sender.Photo,
					"rocket_end_time": strconv.FormatInt(effect.End.Unix(), 10),
				}
			}
		}
		dateGiftResult(w, result, nil)
	}
}

func (a roomGiftEffectsAPI) speed(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Room     int64 `json:"room_id,string"`
		Anchor   int64 `json:"anchor_id,string"`
		User     int64 `json:"user_id,string"`
		DateRoom int64 `json:"date_room_id,string"`
		Likes    int32 `json:"likes_count"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if input.Room < 1 || input.Room != input.DateRoom || input.Likes < 0 {
		httpx.Error(w, 400, "invalid rocket speed request")
		return
	}
	if input.User != actor {
		httpx.LiveError(w, live.ErrForbidden)
		return
	}
	if err := a.store.IncreaseRocketSpeed(r.Context(), input.Room, input.Anchor, actor, input.Likes, a.coefficient); err != nil {
		httpx.LiveError(w, err)
		return
	}
	dateGiftResult(w, "success", nil)
}

func (a roomGiftEffectsAPI) slap(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		User   int64 `json:"userId,string"`
		Room   int64 `json:"roomId,string"`
		Anchor int64 `json:"anchorId,string"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	room, err := a.rooms.Info(r.Context(), input.Room)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	if input.User != actor || input.Anchor != room.OwnerID {
		httpx.LiveError(w, live.ErrForbidden)
		return
	}
	effect, err := a.store.RoomRocket(r.Context(), room.ID, room.Session.LiveID)
	if err != nil {
		httpx.LiveError(w, err)
		return
	}
	if room.Online && effect != nil && effect.Active(time.Now()) {
		dateGiftResult(w, false, nil)
		return
	}
	seen, err := a.store.RocketWasLaunched(r.Context(), room.ID, actor)
	dateGiftResult(w, !seen, err)
}

func (a roomGiftEffectsAPI) end(w http.ResponseWriter, r *http.Request, actor int64) {
	var input struct {
		Anchor int64           `json:"anchorId"`
		Room   int64           `json:"dateRoomId,string"`
		Type   json.RawMessage `json:"giftType"`
	}
	if !httpx.Decode(w, r, &input) {
		return
	}
	if actor != input.Anchor {
		httpx.LiveError(w, live.ErrForbidden)
		return
	}
	var name string
	rocket := json.Unmarshal(input.Type, &name) == nil && name == "rocket"
	var value struct {
		Hardware int64 `json:"hardware"`
	}
	if !rocket && (json.Unmarshal(input.Type, &value) != nil || value.Hardware <= 0) {
		httpx.Error(w, 400, "invalid gift type")
		return
	}
	if err := a.store.EndRoomEffect(r.Context(), input.Room, actor, rocket, value.Hardware); err != nil {
		httpx.LiveError(w, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"status": "200"})
}
