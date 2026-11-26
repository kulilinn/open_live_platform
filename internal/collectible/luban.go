package collectible

import (
	"encoding/json"
	"fmt"
)

var LubanTables = []string{"nft_equity_data", "live_decoration_config", "nft_card"}

type LubanCatalog struct {
	privileges []Privilege
	adorns     []Adorn
	adornByID  map[int32]Adorn
	cards      map[uint32]Card
}

func ReadLubanCatalog(tables map[string][]byte) (*LubanCatalog, error) {
	var privileges []struct {
		ID          int32  `json:"id"`
		Group       int32  `json:"rights_group"`
		Icon        string `json:"rights_icon"`
		Name        string `json:"rights_name"`
		Status      int32  `json:"rights_status"`
		Description string `json:"rights_description"`
		Redirect    string `json:"redirect_page"`
	}
	var adorns []struct {
		ID           int32  `json:"id"`
		Name         string `json:"outfit_name"`
		Description  string `json:"outfit_description"`
		Icon         string `json:"outfit_icon"`
		Unlock       int32  `json:"unlock_condition"`
		Entry        string `json:"entry_effect"`
		Floating     string `json:"entry_floating_screen"`
		AvatarFrame  string `json:"exclusive_avatar_frame"`
		ColoredName  string `json:"colored_name"`
		ChatBubble   string `json:"chat_bubble"`
		Medal        string `json:"exclusive_medal"`
		MedalTag     string `json:"exclusive_medal_tag"`
		EffectTime   int32  `json:"effect_time"`
		FloatingTime int32  `json:"floating_screen_time"`
	}
	var cards []struct {
		ID      uint32 `json:"id"`
		Picture string `json:"card_pic"`
		English string `json:"card_name_en"`
		Chinese string `json:"card_name_zh"`
	}
	records := []any{&privileges, &adorns, &cards}
	for i, name := range LubanTables {
		raw, ok := tables[name]
		if !ok {
			return nil, fmt.Errorf("missing Luban table %s", name)
		}
		if err := json.Unmarshal(raw, records[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	result := &LubanCatalog{adornByID: make(map[int32]Adorn, len(adorns)), cards: make(map[uint32]Card, len(cards))}
	seen := make(map[int32]bool, len(privileges))
	for _, row := range privileges {
		if row.ID <= 0 || seen[row.ID] || row.Name == "" {
			return nil, fmt.Errorf("invalid Luban NFT privilege %d", row.ID)
		}
		seen[row.ID] = true
		result.privileges = append(result.privileges, Privilege(row))
	}
	for _, row := range adorns {
		if _, exists := result.adornByID[row.ID]; exists || row.ID <= 0 || row.EffectTime < 0 || row.FloatingTime < 0 {
			return nil, fmt.Errorf("invalid Luban adorn %d", row.ID)
		}
		item := Adorn(row)
		result.adornByID[item.ID] = item
		result.adorns = append(result.adorns, item)
	}
	for _, row := range cards {
		if _, exists := result.cards[row.ID]; exists {
			return nil, fmt.Errorf("repeated Luban card %d", row.ID)
		}
		result.cards[row.ID] = Card{ID: row.ID, Picture: row.Picture, English: row.English, Chinese: row.Chinese}
	}
	return result, nil
}

func (s *Service) ConfigureLuban(value *LubanCatalog) { s.luban.Store(value) }

func (s *Service) availableAdorns() []Adorn {
	if cfg := s.luban.Load(); cfg != nil {
		return cfg.adorns
	}
	return s.data.Adorns
}

func (s *Service) adornLookup() map[int32]Adorn {
	if cfg := s.luban.Load(); cfg != nil {
		return cfg.adornByID
	}
	return s.adorn
}
