package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
)

type deviceCommand struct {
	Name string          `json:"cmd"`
	Data json.RawMessage `json:"data,omitempty"`
}

type deviceInfo struct {
	Status  string `json:"status"`
	PID     string `json:"pid"`
	ID      string `json:"id"`
	Battery int32  `json:"battery"`
}

type deviceFrame struct {
	Content string        `json:"content"`
	Queue   string        `json:"queue"`
	Payload deviceCommand `json:"payload"`
}

type deviceHub struct {
	private         privateStreams
	mu              sync.Mutex
	users           map[int64]map[*devicePeer]struct{}
	cluster         RustDeviceStore
	scope           string
	node            string
	remote          map[int64]map[string]*remoteDeviceStatus
	sequence        uint64
	controlsEnabled bool
	grants          RustControlStore
	memoryGrants    map[controlGrantKey]controlGrantState
}

type devicePeer struct {
	hub      *deviceHub
	uid      int64
	device   bool
	status   []deviceInfo
	messages chan any
}

func makeDeviceCommand(name string, data any) deviceCommand {
	encoded, _ := json.Marshal(data)
	return deviceCommand{Name: name, Data: encoded}
}

func (h *deviceHub) join(ctx context.Context, uid int64, device bool) *devicePeer {
	h.mu.Lock()
	if h.users == nil {
		h.users = make(map[int64]map[*devicePeer]struct{})
	}
	peers := h.users[uid]
	if peers == nil {
		peers = make(map[*devicePeer]struct{})
		h.users[uid] = peers
	}
	peer := &devicePeer{hub: h, uid: uid, device: device, messages: make(chan any, 32)}
	hadDevice := h.hasDevice(uid)
	peers[peer] = struct{}{}
	if device {
		if !hadDevice {
			peer.toBrowsers(deviceCommand{Name: "connector_connected"})
		}
	} else {
		peer.send(peer.frame("device", peer.deviceStatus()))
	}
	h.mu.Unlock()
	if device {
		h.publishPresence(ctx, uid)
	} else {
		h.broadcast(ctx, uid, "sync", deviceCommand{})
	}
	return peer
}

func (p *devicePeer) close() {
	p.hub.mu.Lock()
	peers := p.hub.users[p.uid]
	delete(peers, p)
	if len(peers) == 0 {
		delete(p.hub.users, p.uid)
		p.hub.forgetRemote(p.uid)
	} else if p.device {
		p.toBrowsers(p.deviceStatus())
		if !p.hub.hasDevice(p.uid) {
			p.toBrowsers(deviceCommand{Name: "connector_disconnected"})
		}
	}
	p.hub.mu.Unlock()
	if p.device {
		p.hub.publishPresence(context.Background(), p.uid)
	}
}

func (p *devicePeer) frame(content string, cmd deviceCommand) deviceFrame {
	return deviceFrame{Content: content, Queue: strconv.FormatInt(p.uid, 10), Payload: cmd}
}

func (p *devicePeer) deviceStatus() deviceCommand {
	status := []deviceInfo{}
	for peer := range p.hub.users[p.uid] {
		status = append(status, peer.status...)
	}
	for _, remote := range p.hub.remote[p.uid] {
		status = append(status, remote.devices...)
	}
	return makeDeviceCommand("devices", status)
}

func (p *devicePeer) toBrowsers(cmd deviceCommand) {
	frame := p.frame("device", cmd)
	for peer := range p.hub.users[p.uid] {
		if !peer.device {
			peer.send(frame)
		}
	}
}

func (p *devicePeer) send(frame any) {
	select {
	case p.messages <- frame:
	default:
		// A blocked connection cannot delay another user's device traffic.
	}
}

func (p *devicePeer) publish(ctx context.Context, target int64, raw string) {
	var cmd deviceCommand
	if json.Unmarshal([]byte(raw), &cmd) != nil {
		return
	}
	p.hub.mu.Lock()
	if p.device {
		switch cmd.Name {
		case "devices":
			var status []deviceInfo
			if json.Unmarshal(cmd.Data, &status) == nil {
				p.status = status
				p.toBrowsers(p.deviceStatus())
				p.hub.mu.Unlock()
				p.hub.publishPresence(ctx, p.uid)
				return
			}
		case "joystick":
			if json.Valid(cmd.Data) {
				p.toBrowsers(cmd)
				p.hub.mu.Unlock()
				p.hub.broadcast(ctx, p.uid, "joystick", cmd)
				p.forwardJoystick(ctx, cmd)
				return
			}
		case "heartbeat":
			p.send(deviceCommand{Name: "heartbeat"})
		}
		p.hub.mu.Unlock()
		return
	}
	if cmd.Name == "heartbeat" {
		p.send(p.frame("device", cmd))
		p.hub.mu.Unlock()
		return
	}
	p.hub.mu.Unlock()
	switch cmd.Name {
	case "vibrate", "mode_vibrate", "mode_vibrate_v2", "motor_strength":
		p.hub.routeControl(ctx, p.uid, target, cmd)
	}
}

func (p *devicePeer) forwardJoystick(ctx context.Context, cmd deviceCommand) {
	var joystick struct {
		Device string
		Event  string
		Value  int32
	}
	if json.Unmarshal(cmd.Data, &joystick) != nil || !strings.EqualFold(joystick.Event, "motion") {
		return
	}
	grant, ok, err := p.hub.activeGrant(ctx, p.uid, 0)
	if err == nil && ok {
		vibrate := makeDeviceCommand("vibrate", map[string]any{"device": joystick.Device, "primary": joystick.Value, "secondary": 0, "duration": 5})
		_ = p.hub.emitControl(ctx, grant.ControlledID, vibrate)
	}
}

func (h *deviceHub) sendControl(uid int64, cmd deviceCommand) {
	for peer := range h.users[uid] {
		if peer.device {
			if control, ok := peer.control(cmd); ok {
				peer.send(control)
			}
		}
	}
}

func (h *deviceHub) sync(ctx context.Context, uid int64) {
	h.mu.Lock()
	for peer := range h.users[uid] {
		peer.toBrowsers(peer.deviceStatus())
		break
	}
	h.mu.Unlock()
	h.broadcast(ctx, uid, "sync", deviceCommand{})
}

func (p *devicePeer) control(cmd deviceCommand) (deviceCommand, bool) {
	type motorData struct {
		Device    string `json:"device"`
		Primary   int32  `json:"primary"`
		Secondary int32  `json:"secondary"`
		Duration  int32  `json:"duration"`
	}
	switch cmd.Name {
	case "vibrate", "mode_vibrate", "mode_vibrate_v2", "motor_strength":
	default:
		return deviceCommand{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(cmd.Data, &fields) != nil || fields == nil {
		return deviceCommand{}, false
	}
	var device string
	if field, ok := fields["device"]; ok && json.Unmarshal(field, &device) != nil {
		return deviceCommand{}, false
	}
	if device == "" {
		for _, status := range p.status {
			if status.Status == "connected" {
				device = status.ID
			}
		}
	}
	if device == "" {
		return deviceCommand{}, false
	}
	if cmd.Name == "vibrate" {
		data := motorData{Duration: 5}
		if len(fields["primary"]) == 0 || len(fields["secondary"]) == 0 || json.Unmarshal(cmd.Data, &data) != nil {
			return deviceCommand{}, false
		}
		data.Device = device
		data.Primary = min(max(data.Primary, -100), 100)
		data.Secondary = min(max(data.Secondary, -100), 100)
		data.Duration = min(max(data.Duration, 0), 600000)
		return makeDeviceCommand(cmd.Name, data), true
	}
	fields["device"], _ = json.Marshal(device)
	return makeDeviceCommand(cmd.Name, fields), true
}

func parseRustH5Command(raw json.RawMessage, roomID, uid int64) (command, bool) {
	var frame struct {
		Content string          `json:"content"`
		Queue   string          `json:"queue"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return command{}, false
	}
	if frame.Content == "live_chat" && roomID != 0 {
		return parseH5Command(raw, roomID, uid)
	}
	if frame.Content != "control" && frame.Content != "device" {
		return command{}, false
	}
	var target int64
	if frame.Queue != "" {
		var err error
		target, err = strconv.ParseInt(frame.Queue, 10, 64)
		if err != nil || target <= 0 {
			return command{}, false
		}
	}
	var embedded string
	if json.Unmarshal(frame.Payload, &embedded) == nil {
		frame.Payload = json.RawMessage(embedded)
	}
	var frames []json.RawMessage
	if json.Unmarshal(frame.Payload, &frames) == nil {
		if len(frames) != 1 {
			return command{}, false
		}
		frame.Payload = frames[0]
	}
	cmd, ok := parseDeviceCommand(frame.Payload)
	cmd.DeviceTarget = target
	return cmd, ok
}
