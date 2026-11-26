package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
)

// PushPrivate sends a Rust ChannelMessage to the recipient's H5 connections.
type PushPrivate func(context.Context, int64, string, any) error

type privateStreams struct {
	mu    sync.Mutex
	users map[int64]map[chan json.RawMessage]struct{}
}

func (p *privateStreams) subscribe(id int64) (<-chan json.RawMessage, func()) {
	ch := make(chan json.RawMessage, 32)
	p.mu.Lock()
	if p.users == nil {
		p.users = make(map[int64]map[chan json.RawMessage]struct{})
	}
	if p.users[id] == nil {
		p.users[id] = make(map[chan json.RawMessage]struct{})
	}
	p.users[id][ch] = struct{}{}
	p.mu.Unlock()
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.users[id], ch)
		if len(p.users[id]) == 0 {
			delete(p.users, id)
		}
	}
}

func (p *privateStreams) deliver(id int64, frame json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for ch := range p.users[id] {
		select {
		case ch <- frame:
		default:
		} // Offline or slow readers recover from the inbox.
	}
}

func (h *deviceHub) pushPrivate(ctx context.Context, id int64, content string, payload any) error {
	frame, err := json.Marshal(struct {
		Content string `json:"content"`
		Queue   string `json:"queue"`
		Payload any    `json:"payload"`
	}{content, strconv.FormatInt(id, 10), payload})
	if err != nil {
		return err
	}
	if h.cluster != nil {
		if err := h.cluster.PublishRustDeviceSignal(ctx, RustDeviceSignal{Scope: h.scope, Node: h.node, UserID: id, Kind: "private", Data: frame}); err != nil {
			return err
		}
	}
	h.private.deliver(id, frame)
	return nil
}
