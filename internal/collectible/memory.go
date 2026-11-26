package collectible

import (
	"context"
	"maps"
	"sync"
	"time"
)

type Memory struct {
	mu         sync.Mutex
	challenges map[int64]Challenge
	state      map[int64]State
	owners     map[uint32]int64
	cards      map[uint32]Card
}

func NewMemory() *Memory {
	return &Memory{challenges: make(map[int64]Challenge), state: make(map[int64]State),
		owners: make(map[uint32]int64), cards: make(map[uint32]Card)}
}

func (s *Memory) SaveNFTChallenge(_ context.Context, c Challenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, saved := range s.challenges {
		if !saved.Expires.After(now) {
			delete(s.challenges, id)
		}
	}
	s.challenges[c.ID] = c
	return nil
}

func (s *Memory) NFTChallenge(_ context.Context, userID, id int64) (Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.challenge(userID, id)
}

func (s *Memory) challenge(userID, id int64) (Challenge, error) {
	c, ok := s.challenges[id]
	if !ok || c.UserID != userID || !c.Expires.After(time.Now()) {
		return Challenge{}, ErrSignature
	}
	return c, nil
}

func (s *Memory) NFTState(_ context.Context, userID int64) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state[userID]
	state.Slots = maps.Clone(state.Slots)
	return state, nil
}

func (s *Memory) BindNFT(_ context.Context, c Challenge, binding Binding, slots map[int8]int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	saved, err := s.challenge(c.UserID, c.ID)
	if err != nil || saved.Message != c.Message {
		return ErrSignature
	}
	if s.state[c.UserID].Binding != nil {
		return ErrAlready
	}
	if ownerID, exists := s.owners[binding.TokenID]; exists {
		delete(s.state, ownerID)
	}
	s.state[c.UserID] = State{Binding: &binding, Slots: maps.Clone(slots)}
	s.owners[binding.TokenID] = c.UserID
	delete(s.challenges, c.ID)
	return nil
}

func (s *Memory) UnbindNFT(_ context.Context, userID int64, expected *Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state[userID]
	if expected != nil && (state.Binding == nil || *state.Binding != *expected) {
		return nil
	}
	if state.Binding == nil {
		return ErrUnbound
	}
	delete(s.owners, state.Binding.TokenID)
	delete(s.state, userID)
	return nil
}

func (s *Memory) EquipNFT(_ context.Context, userID int64, binding Binding, slots map[int8]int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state[userID]
	if state.Binding == nil || *state.Binding != binding {
		return ErrEquip
	}
	if state.Slots == nil {
		state.Slots = make(map[int8]int32)
		s.state[userID] = state
	}
	maps.Copy(state.Slots, slots)
	return nil
}

func (s *Memory) UnequipNFT(_ context.Context, userID int64, slot int8, id int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := s.state[userID].Slots
	if slot != 0 {
		delete(slots, slot)
		return nil
	}
	for key, adornID := range slots {
		if adornID == id {
			delete(slots, key)
		}
	}
	return nil
}

func (s *Memory) NFTCards(_ context.Context) ([]Card, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cards := make([]Card, 0, len(s.cards))
	for _, card := range s.cards {
		cards = append(cards, card)
	}
	return cards, nil
}

func (s *Memory) SaveNFTCards(_ context.Context, cards []Card, sold bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, card := range cards {
		current, exists := s.cards[card.ID]
		if !exists || (sold && current.Status != 1) {
			s.cards[card.ID] = card
		}
	}
	return nil
}
