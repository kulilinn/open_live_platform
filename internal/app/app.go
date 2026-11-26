package app

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/edge"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/rtc"
	"open_live_platform/internal/store/postgres"
)

type Config struct {
	DatabaseURL      string
	Addr             string
	GRPCAddr         string
	AdminToken       string
	EdgeToken        string
	RelaySecret      string
	RelayPublicURL   string
	GiftCatalogFile  string
	JWTSecret        string
	AgoraAppID       string
	AgoraCertificate string
	AgoraTokenExpire string

	AnchorAwayTimeout time.Duration
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func ConfigFromEnv() Config {
	adminToken := env("ADMIN_TOKEN", "admin")
	return Config{
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		Addr:              env("PLATFORM_ADDR", ":8080"),
		GRPCAddr:          os.Getenv("PLATFORM_GRPC_ADDR"),
		AdminToken:        adminToken,
		EdgeToken:         env("EDGE_TOKEN", adminToken),
		RelaySecret:       env("RELAY_SECRET", "dev-secret"),
		RelayPublicURL:    os.Getenv("RELAY_PUBLIC_URL"),
		GiftCatalogFile:   os.Getenv("GIFT_CATALOG_FILE"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		AgoraAppID:        os.Getenv("AGORA_APP_ID"),
		AgoraCertificate:  os.Getenv("AGORA_APP_CERTIFICATE"),
		AgoraTokenExpire:  os.Getenv("AGORA_TOKEN_EXPIRE"),
		AnchorAwayTimeout: anchorAwayTimeoutFromEnv(),
		ReadTimeout:       duration("PLATFORM_READ_TIMEOUT", 15*time.Second),
		ReadHeaderTimeout: duration("PLATFORM_READ_HEADER_TIMEOUT", 5*time.Second),
		WriteTimeout:      duration("PLATFORM_WRITE_TIMEOUT", 15*time.Second),
		IdleTimeout:       duration("PLATFORM_IDLE_TIMEOUT", 60*time.Second),
	}
}

type App struct {
	Config   Config
	Accounts auth.AccountStore
	Rooms    live.RoomStore
	Server   *http.Server
	db       *postgres.Store
}

func New(ctx context.Context, config Config) (*App, error) {
	config.GRPCAddr = cmp.Or(config.GRPCAddr, "127.0.0.1:9090")
	if config.AnchorAwayTimeout <= 0 {
		config.AnchorAwayTimeout = live.DefaultAnchorAwayTimeout
	}
	signer, err := rtc.NewSigner(config.AgoraAppID, config.AgoraCertificate, config.AgoraTokenExpire)
	if err != nil {
		return nil, err
	}
	gifts, err := gift.Load(config.GiftCatalogFile)
	if err != nil {
		return nil, fmt.Errorf("gift catalog: %w", err)
	}
	var backend auth.JWTBackend
	var rooms live.RoomStore
	var db *postgres.Store
	if config.DatabaseURL == "" {
		backend, rooms = auth.NewStore(), live.NewStoreWithAnchorTimeout(config.AnchorAwayTimeout)
	} else {
		var err error
		db, err = postgres.OpenWithAnchorTimeout(ctx, config.DatabaseURL, config.AnchorAwayTimeout)
		if err != nil {
			return nil, err
		}
		backend, rooms = db, db
	}
	accounts := auth.WithJWT(backend, config.JWTSecret)
	mux := http.NewServeMux()

	httpapi.RegisterWithRTC(mux, accounts, rooms, signer)
	httpapi.RegisterGiftCatalog(mux, accounts, gifts)
	admin.Register(mux, accounts, rooms, config.AdminToken)
	edge.Register(mux, rooms, config.EdgeToken)
	relay.Register(mux, accounts, rooms, config.RelaySecret, config.RelayPublicURL)

	return &App{
		db:       db,
		Config:   config,
		Accounts: accounts,
		Rooms:    rooms,
		Server: &http.Server{
			Addr:              config.Addr,
			Handler:           mux,
			ReadTimeout:       config.ReadTimeout,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
		},
	}, nil
}

func (a *App) Close() {
	if a.db != nil {
		a.db.Shutdown()
	}
}

func anchorAwayTimeoutFromEnv() time.Duration {
	seconds, err := strconv.ParseUint(os.Getenv("TEMP_OFFLINE_TIMEOUT"), 10, 32)
	if err != nil || seconds == 0 {
		return live.DefaultAnchorAwayTimeout
	}
	return time.Duration(seconds) * time.Second
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
