package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
)

// PushPrivate sends a Rust ChannelMessage to the recipient's H5 connections.
type PushPrivate func(context.Context, int64, string, any) error

// QueuedPayload preserves the sender's H5 queue when the same frame is delivered to both participants.
type QueuedPayload struct {
	Queue   int64
	Payload any
}

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

func (p *privateStreams) deliverGlobal(payload json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, streams := range p.users {
		frame, err := json.Marshal(struct {
			Content string          `json:"content"`
			Queue   string          `json:"queue"`
			Payload json.RawMessage `json:"payload"`
		}{"notification", strconv.FormatInt(id, 10), payload})
		if err != nil {
			return
		}
		for ch := range streams {
			select {
			case ch <- frame:
			default:
			}
		}
	}
}

func (h *deviceHub) pushPrivate(ctx context.Context, id int64, content string, payload any) error {
	queue := id
	if msg, ok := payload.(QueuedPayload); ok {
		queue, payload = msg.Queue, msg.Payload
	}
	frame, err := PrivateFrame(queue, content, payload)
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

func PrivateFrame(queue int64, content string, payload any) (json.RawMessage, error) {
	return json.Marshal(struct {
		Content string `json:"content"`
		Queue   string `json:"queue"`
		Payload any    `json:"payload"`
	}{content, strconv.FormatInt(queue, 10), payload})
}
