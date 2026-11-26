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
	Daily   []string          `json:"daily"`
}

type Config struct {
	DatabasePool                           bool              `json:"-"`
	DailyChat                              bool              `json:"-"`
	Accounts                               []Account         `json:"accounts"`
	Scripts                                map[string]Script `json:"scripts"`
	RoomPopulation                         int               `json:"room_population"`
	EntrySeconds                           int               `json:"entry_seconds"`
	StaySeconds                            int               `json:"stay_seconds"`
	WelcomeDelaySeconds                    int               `json:"welcome_delay_seconds"`
	GiftDelaySeconds                       int               `json:"gift_delay_seconds"`
	LikeDelaySeconds                       *int              `json:"like_delay_seconds"`
	DailyDelaySeconds                      *int              `json:"daily_delay_seconds"`
	MinimumGiftPoints                      int64             `json:"-"`
	population, entry, stay, welcome, gift []weightedValue
	like, daily                            []weightedValue
}

func WantsLuban() bool {
	_, enabled := os.LookupEnv("USE_LIVEROOM_BOT")
	return enabled && os.Getenv("LIVEROOM_BOT_FILE") == "" && os.Getenv("ETCD_CONFIG_PREFIX") != ""
}

func LoadFromEnv() (Config, error) {
	if _, enabled := os.LookupEnv("USE_LIVEROOM_BOT"); !enabled {
		return Config{}, nil
	}
	file := os.Getenv("LIVEROOM_BOT_FILE")
	cfg, err := defaults()
	if err != nil {
		return Config{}, err
	}
	if file == "" {
		if WantsLuban() || cfg.DatabasePool {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("USE_LIVEROOM_BOT requires LIVEROOM_BOT_FILE")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	if len(cfg.Accounts) == 0 && !cfg.DatabasePool {
		return Config{}, fmt.Errorf("live room bots require at least one account")
	}
	return cfg, nil
}

func defaults() (Config, error) {
	cfg := Config{RoomPopulation: 20, EntrySeconds: 30, StaySeconds: 600, WelcomeDelaySeconds: 180, GiftDelaySeconds: 1, MinimumGiftPoints: 100}
	cfg.DatabasePool = os.Getenv("LIVEROOM_BOT_DB_POOL") == "1"
	cfg.DailyChat = os.Getenv("DAILY_CHAT_OPEN") == "1"
	if value, supplied := os.LookupEnv("BOT_GIFT_PRICE"); supplied {
		points, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid BOT_GIFT_PRICE: %w", err)
		}
		cfg.MinimumGiftPoints = points
	}
	return cfg, nil
}

func (c Config) Enabled() bool {
	return c.DatabasePool || len(c.Accounts) > 0
}

func (c Config) AccountIDs() ([]int64, error) {
	if c.RoomPopulation < 1 || c.EntrySeconds < 0 || c.EntrySeconds > 86400 ||
		c.StaySeconds < 1 || c.StaySeconds > 86400 ||
		c.WelcomeDelaySeconds < 0 || c.WelcomeDelaySeconds > 86400 ||
		c.GiftDelaySeconds < 0 || c.GiftDelaySeconds > 86400 || c.MinimumGiftPoints < 0 {
		return nil, fmt.Errorf("invalid live room bot settings")
	}
	for _, delay := range []*int{c.LikeDelaySeconds, c.DailyDelaySeconds} {
		if delay != nil && (*delay < 0 || *delay > 86400) {
			return nil, fmt.Errorf("invalid live room bot action delay")
		}
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
		if !validate(script.Welcome) || !validate(script.Daily) {
			return nil, fmt.Errorf("invalid live room bot welcome or daily text for %s", locale)
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
	script := c.script(accountID)
	lines, delay := script.Welcome, pickWeighted(c.welcome, c.WelcomeDelaySeconds, rand.Uint64())
	if giftType != nil {
		lines, delay = script.Gifts[*giftType], pickWeighted(c.gift, c.GiftDelaySeconds, rand.Uint64())
	}
	if text := randomLine(lines); text != "" {
		return text, time.Duration(delay) * time.Second
	}
	return "", 0
}

func (c Config) DailyReply(accountID int64) (string, time.Duration) {
	delay := rand.IntN(6)
	if c.DailyDelaySeconds != nil {
		delay = *c.DailyDelaySeconds
	}
	delay = pickWeighted(c.daily, delay, rand.Uint64())
	return randomLine(c.script(accountID).Daily), time.Duration(max(1, delay)) * time.Second
}

func (c Config) script(accountID int64) Script {
	for _, account := range c.Accounts {
		if account.ID == accountID {
			return c.Scripts[strings.ToLower(strings.TrimSpace(account.Locale))]
		}
	}
	if c.DatabasePool {
		return c.Scripts["ja"]
	}
	return Script{}
}

func randomLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[rand.IntN(len(lines))]
}
