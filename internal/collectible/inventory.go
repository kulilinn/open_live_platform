package collectible

import (
	"context"
	"crypto/subtle"

	"golang.org/x/sync/errgroup"
)

func (s *Service) AuthorizedSync(authorization string) bool {
	return s.syncToken != "" && subtle.ConstantTimeCompare([]byte(authorization), []byte("Bearer "+s.syncToken)) == 1
}

func (s *Service) cards(ctx context.Context) (map[uint32]Card, error) {
	updates, err := s.store.NFTCards(ctx)
	if err != nil {
		return nil, err
	}
	cards := make(map[uint32]Card, len(s.data.Cards)+len(updates))
	for _, card := range s.data.Cards {
		cards[card.ID] = card
	}
	for _, card := range updates {
		cards[card.ID] = card
	}
	if cfg := s.luban.Load(); cfg != nil {
		for id, card := range cards {
			if meta, ok := cfg.cards[id]; ok {
				card.Picture, card.English, card.Chinese = meta.Picture, meta.English, meta.Chinese
				cards[id] = card
			}
		}
	}
	return cards, nil
}

func (s *Service) SyncCards(ctx context.Context, initial bool) error {
	if s.chain == nil || s.treasury == "" {
		return ErrUnavailable
	}
	cards, err := s.cards(ctx)
	if err != nil {
		return err
	}
	block, err := s.chain.finalizedBlock(ctx)
	if err != nil {
		return err
	}
	available, err := s.chain.inventory(ctx, s.treasury, block)
	if err != nil {
		return err
	}
	if initial {
		return s.initializeCards(ctx, cards, available)
	}
	return s.refreshCards(ctx, cards, available, block)
}

func (s *Service) initializeCards(ctx context.Context, existing map[uint32]Card, available map[uint32]struct{}) error {
	newCards := make([]Card, 0)
	for _, card := range s.data.Inventory {
		_, found := existing[card.ID]
		_, minted := available[card.ID]
		if !found && minted {
			newCards = append(newCards, card)
		}
	}
	return s.store.SaveNFTCards(ctx, newCards, false)
}

func (s *Service) refreshCards(ctx context.Context, existing map[uint32]Card, available map[uint32]struct{}, block string) error {
	candidates := make([]Card, 0)
	for _, card := range existing {
		if _, found := available[card.ID]; !found && card.Status != 1 {
			candidates = append(candidates, card)
		}
	}
	requests, queryCtx := errgroup.WithContext(ctx)
	requests.SetLimit(8)
	for index := range candidates {
		requests.Go(func() error {
			card := &candidates[index]
			owner, err := s.chain.ownerAt(queryCtx, card.ID, block)
			if err != nil {
				return err
			}
			if owner == s.treasury {
				// The two reads at one block must agree. Retry from a trusted RPC.
				return ErrUnavailable
			}
			card.Address, card.Status = owner, 1
			return nil
		})
	}
	if err := requests.Wait(); err != nil {
		return err
	}
	return s.store.SaveNFTCards(ctx, candidates, true)
}
