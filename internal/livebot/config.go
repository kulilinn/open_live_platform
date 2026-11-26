package livebot

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Account struct {
	ID     int64  `json:"id,string"`
	Locale string `json:"locale"`
}

type Script struct {
	Welcome []string          `json:"welcome"`
	Gifts   map[int8][]string `json:"gifts"`
}

type Config struct {
	Accounts            []Account         `json:"accounts"`
	Scripts             map[string]Script `json:"scripts"`
	RoomPopulation      int               `json:"room_population"`
	EntrySeconds        int               `json:"entry_seconds"`
	StaySeconds         int               `json:"stay_seconds"`
	WelcomeDelaySeconds int               `json:"welcome_delay_seconds"`
	GiftDelaySeconds    int               `json:"gift_delay_seconds"`
	MinimumGiftPoints   int64             `json:"-"`
}

func LoadFromEnv() (Config, error) {
	if _, enabled := os.LookupEnv("USE_LIVEROOM_BOT"); !enabled {
		return Config{}, nil
	}
	file := os.Getenv("LIVEROOM_BOT_FILE")
	if file == "" {
		return Config{}, fmt.Errorf("USE_LIVEROOM_BOT requires LIVEROOM_BOT_FILE")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		RoomPopulation: 20, EntrySeconds: 30, StaySeconds: 600,
		WelcomeDelaySeconds: 180, GiftDelaySeconds: 1, MinimumGiftPoints: 100,
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	if value, supplied := os.LookupEnv("BOT_GIFT_PRICE"); supplied {
		cfg.MinimumGiftPoints, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid BOT_GIFT_PRICE: %w", err)
		}
	}
	if len(cfg.Accounts) == 0 {
		return Config{}, fmt.Errorf("live room bots require at least one account")
	}
	return cfg, nil
}

func (c Config) AccountIDs() ([]int64, error) {
	if c.RoomPopulation < 1 || c.EntrySeconds < 0 || c.EntrySeconds > 86400 ||
		c.StaySeconds < 1 || c.StaySeconds > 86400 ||
		c.WelcomeDelaySeconds < 0 || c.WelcomeDelaySeconds > 86400 ||
		c.GiftDelaySeconds < 0 || c.GiftDelaySeconds > 86400 || c.MinimumGiftPoints < 0 {
		return nil, fmt.Errorf("invalid live room bot settings")
	}
	ids := make([]int64, 0, len(c.Accounts))
	seen := make(map[int64]bool, len(c.Accounts))
	for _, account := range c.Accounts {
		if account.ID < 1 || seen[account.ID] || strings.TrimSpace(account.Locale) == "" {
			return nil, fmt.Errorf("invalid or repeated live room bot account %d", account.ID)
		}
		seen[account.ID] = true
		ids = append(ids, account.ID)
	}
	validate := func(lines []string) bool {
		for _, line := range lines {
			if strings.TrimSpace(line) == "" || utf8.RuneCountInString(line) > 500 || strings.ContainsRune(line, 0) {
				return false
			}
		}
		return true
	}
	for locale, script := range c.Scripts {
		if !validate(script.Welcome) {
			return nil, fmt.Errorf("invalid live room bot welcome text for %s", locale)
		}
		for giftType, lines := range script.Gifts {
			if !validate(lines) {
				return nil, fmt.Errorf("invalid live room bot gift text for %s, gift type %d", locale, giftType)
			}
		}
	}
	return ids, nil
}

func (c Config) Reply(accountID int64, giftType *int8) (string, time.Duration) {
	for _, account := range c.Accounts {
		if account.ID != accountID {
			continue
		}
		locale := strings.ToLower(strings.TrimSpace(account.Locale))
		script := c.Scripts[locale]
		lines, delay := script.Welcome, c.WelcomeDelaySeconds
		if giftType != nil {
			lines, delay = script.Gifts[*giftType], c.GiftDelaySeconds
		}
		if len(lines) > 0 {
			return lines[rand.IntN(len(lines))], time.Duration(delay) * time.Second
		}
		break
	}
	return "", 0
}
