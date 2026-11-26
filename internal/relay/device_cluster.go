package relay

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"slices"
	"time"
)

type RustDeviceStore interface {
	PublishRustDeviceSignal(context.Context, RustDeviceSignal) error
	ReceiveRustDeviceSignals(func(RustDeviceSignal))
}

type RustDeviceSignal struct {
	Scope  string          `json:"scope"`
	Node   string          `json:"node"`
	UserID int64           `json:"uid"`
	Kind   string          `json:"kind"`
	Data   json.RawMessage `json:"data,omitempty"`
}

type remoteDeviceStatus struct {
	sequence uint64
	online   bool
	devices  []deviceInfo
	timer    *time.Timer
}

const remoteDeviceTimeout = 75 * time.Second

func (h *deviceHub) configure(options RustOptions) {
	if options.Devices == nil {
		return
	}
	h.scope = options.AppKey
	h.node = rand.Text()
	h.cluster = options.Devices
	h.remote = make(map[int64]map[string]*remoteDeviceStatus)
	h.cluster.ReceiveRustDeviceSignals(h.receive)
}

func (h *deviceHub) hasDevice(uid int64) bool {
	for peer := range h.users[uid] {
		if peer.device {
			return true
		}
	}
	for _, remote := range h.remote[uid] {
		if remote.online {
			return true
		}
	}
	return false
}

func (h *deviceHub) forgetRemote(uid int64) {
	for _, remote := range h.remote[uid] {
		remote.timer.Stop()
	}
	delete(h.remote, uid)
}

type devicePresenceSignal struct {
	Online   bool         `json:"online"`
	Devices  []deviceInfo `json:"devices,omitempty"`
	Sequence uint64       `json:"sequence"`
}

func (h *deviceHub) publishPresence(ctx context.Context, uid int64) {
	if h.cluster == nil {
		return
	}
	h.mu.Lock()
	h.sequence++
	presence := devicePresenceSignal{Sequence: h.sequence}
	for peer := range h.users[uid] {
		if peer.device {
			presence.Online = true
			presence.Devices = append(presence.Devices, peer.status...)
		}
	}
	h.mu.Unlock()
	h.broadcast(ctx, uid, "presence", presence)
}

func (h *deviceHub) broadcast(ctx context.Context, uid int64, kind string, payload any) {
	if h.cluster == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = h.cluster.PublishRustDeviceSignal(ctx, RustDeviceSignal{Scope: h.scope, Node: h.node, UserID: uid, Kind: kind, Data: data})
}

func (h *deviceHub) receive(signal RustDeviceSignal) {
	if signal.Scope != h.scope || signal.Node == h.node {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	peers := h.users[signal.UserID]
	if len(peers) == 0 {
		return
	}
	var userPeer *devicePeer
	for peer := range peers {
		userPeer = peer
		break
	}
	switch signal.Kind {
	case "presence":
		var presence devicePresenceSignal
		if json.Unmarshal(signal.Data, &presence) == nil {
			h.remotePresence(signal, userPeer, presence)
		}
	case "sync":
		for peer := range peers {
			if peer.device {
				go h.publishPresence(context.Background(), signal.UserID)
				break
			}
		}
	case "control", "joystick":
		var cmd deviceCommand
		if json.Unmarshal(signal.Data, &cmd) != nil {
			return
		}
		if signal.Kind == "control" {
			h.sendControl(signal.UserID, cmd)
		} else if cmd.Name == "joystick" {
			userPeer.toBrowsers(cmd)
		}
	}
}

func (h *deviceHub) remotePresence(signal RustDeviceSignal, peer *devicePeer, presence devicePresenceSignal) {
	nodes := h.remote[signal.UserID]
	if nodes == nil {
		nodes = make(map[string]*remoteDeviceStatus)
		h.remote[signal.UserID] = nodes
	}
	prev := nodes[signal.Node]
	if prev != nil && prev.sequence >= presence.Sequence {
		return
	}
	hadDevice := h.hasDevice(signal.UserID)
	if prev != nil {
		prev.timer.Stop()
	}
	current := &remoteDeviceStatus{sequence: presence.Sequence, online: presence.Online, devices: presence.Devices}
	nodes[signal.Node] = current
	current.timer = time.AfterFunc(remoteDeviceTimeout, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.remote[signal.UserID][signal.Node] != current {
			return
		}
		delete(h.remote[signal.UserID], signal.Node)
		if current.online {
			peer.toBrowsers(peer.deviceStatus())
			if !h.hasDevice(signal.UserID) {
				peer.toBrowsers(deviceCommand{Name: "connector_disconnected"})
			}
		}
	})
	if !hadDevice && presence.Online {
		peer.toBrowsers(deviceCommand{Name: "connector_connected"})
	}
	if (prev == nil && presence.Online) || (prev != nil && !slices.Equal(prev.devices, presence.Devices)) {
		peer.toBrowsers(peer.deviceStatus())
	}
	if hadDevice && !h.hasDevice(signal.UserID) {
		peer.toBrowsers(deviceCommand{Name: "connector_disconnected"})
	}
}
