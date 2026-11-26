package livebot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

var LubanTables = []string{
	"robot_create_property", "robot_welcome_message_text", "robot_interactive_gift_text",
	"robot_room_population", "robot_room_entry_time", "robot_room_exit_time",
	"robot_welcome_message_time", "robot_interactive_gift_time",
	"robot_like_time", "robot_daily_time", "robot_daily_text",
}

type weightedValue struct {
	Weight     int `json:"weight"`
	Time       int `json:"time"`
	Population int `json:"population"`
}

func FromLuban(base Config, tables map[string][]byte) (Config, error) {
	var accounts []struct {
		UID    int64  `json:"uid"`
		Locale string `json:"location"`
	}
	var welcome []struct {
		Locale string `json:"weight"`
		Text   string `json:"text"`
	}
	var gifts []struct {
		Locale string `json:"weight"`
		Text   string `json:"text"`
		Kind   int8   `json:"gift_type"`
	}
	for name, dst := range map[string]any{LubanTables[0]: &accounts, LubanTables[1]: &welcome, LubanTables[2]: &gifts} {
		data, ok := tables[name]
		if !ok {
			return Config{}, fmt.Errorf("missing Luban table %s", name)
		}
		if err := json.Unmarshal(data, dst); err != nil {
			return Config{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	cfg := base
	if len(accounts) == 0 && !cfg.DatabasePool {
		return Config{}, fmt.Errorf("Luban robot account table is empty")
	}
	cfg.Accounts = make([]Account, 0, len(accounts))
	for _, user := range accounts {
		cfg.Accounts = append(cfg.Accounts, Account{ID: user.UID, Locale: user.Locale})
	}
	cfg.Scripts = make(map[string]Script)
	for _, line := range welcome {
		key := strings.ToLower(strings.TrimSpace(line.Locale))
		script := cfg.Scripts[key]
		script.Welcome = append(script.Welcome, line.Text)
		cfg.Scripts[key] = script
	}
	for _, line := range gifts {
		key := strings.ToLower(strings.TrimSpace(line.Locale))
		script := cfg.Scripts[key]
		if script.Gifts == nil {
			script.Gifts = make(map[int8][]string)
		}
		script.Gifts[line.Kind] = append(script.Gifts[line.Kind], line.Text)
		cfg.Scripts[key] = script
	}
	if data, found := tables["robot_daily_text"]; found {
		var daily []struct {
			Locale string `json:"weight"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(data, &daily); err != nil {
			return Config{}, fmt.Errorf("robot_daily_text: %w", err)
		}
		for _, line := range daily {
			key := strings.ToLower(strings.TrimSpace(line.Locale))
			script := cfg.Scripts[key]
			script.Daily = append(script.Daily, line.Text)
			cfg.Scripts[key] = script
		}
	}
	rules := []*[]weightedValue{&cfg.population, &cfg.entry, &cfg.stay, &cfg.welcome, &cfg.gift, &cfg.like, &cfg.daily}
	for i, table := range LubanTables[3 : 3+len(rules)] {
		if data, ok := tables[table]; ok {
			if err := json.Unmarshal(data, rules[i]); err != nil {
				return Config{}, fmt.Errorf("%s: %w", table, err)
			}
			for j, row := range *rules[i] {
				value := row.Time
				if i == 0 {
					value = row.Population
				}
				if value < 0 || value > 86400 || row.Weight < 0 || (i == 0 || i == 2) && value == 0 {
					return Config{}, fmt.Errorf("invalid Luban row %d of %s", j+1, table)
				}
				(*rules[i])[j].Time = value
			}
		}
	}
	_, err := cfg.AccountIDs()
	return cfg, err
}

func pickWeighted(choices []weightedValue, fallback int, seed uint64) int {
	var total uint64
	for _, option := range choices {
		total += uint64(option.Weight)
	}
	if total == 0 {
		return fallback
	}
	index := seed % total
	for _, option := range choices {
		if index < uint64(option.Weight) {
			return option.Time
		}
		index -= uint64(option.Weight)
	}
	return fallback
}

func (c Config) RoomSchedule(liveID int64) (population, entry, stay int) {
	seed := uint64(liveID)
	return pickWeighted(c.population, c.RoomPopulation, mix(seed)),
		pickWeighted(c.entry, c.EntrySeconds, mix(seed+1)), pickWeighted(c.stay, c.StaySeconds, mix(seed+2))
}

func (c Config) LikeDelay(liveID, userID int64) time.Duration {
	seed := mix(uint64(liveID) + mix(uint64(userID)))
	fallback := 600 + int(seed%600)
	if c.LikeDelaySeconds != nil {
		fallback = *c.LikeDelaySeconds
	}
	return time.Duration(pickWeighted(c.like, fallback, seed)) * time.Second
}

func (c Config) CandidateSchedule() (maxPopulation, minEntry int) {
	maxPopulation, minEntry = c.RoomPopulation, c.EntrySeconds
	for _, rule := range c.population {
		if rule.Weight > 0 {
			maxPopulation = max(maxPopulation, rule.Time)
		}
	}
	for _, rule := range c.entry {
		if rule.Weight > 0 {
			minEntry = min(minEntry, rule.Time)
		}
	}
	return maxPopulation, minEntry
}

func mix(seed uint64) uint64 {
	seed = (seed ^ (seed >> 30)) * 0xbf58476d1ce4e5b9
	seed = (seed ^ (seed >> 27)) * 0x94d049bb133111eb
	return seed ^ (seed >> 31)
}
