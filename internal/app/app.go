package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/edge"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/httpapi"
	"open_live_platform/internal/live"
	"open_live_platform/internal/relay"
	"open_live_platform/internal/rpc"
	"open_live_platform/internal/store/postgres"
)

type Config struct {
	DatabaseURL     string
	Addr            string
	AdminToken      string
	EdgeToken       string
	RelaySecret     string
	RelayPublicURL  string
	GiftCatalogFile string
	JWTSecret       string

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
		AdminToken:        adminToken,
		EdgeToken:         env("EDGE_TOKEN", adminToken),
		RelaySecret:       env("RELAY_SECRET", "dev-secret"),
		RelayPublicURL:    os.Getenv("RELAY_PUBLIC_URL"),
		GiftCatalogFile:   os.Getenv("GIFT_CATALOG_FILE"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
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
	gifts, err := gift.Load(config.GiftCatalogFile)
	if err != nil {
		return nil, fmt.Errorf("gift catalog: %w", err)
	}
	var backend auth.JWTBackend
	var rooms live.RoomStore
	var db *postgres.Store
	if config.DatabaseURL == "" {
		backend, rooms = auth.NewStore(), live.NewStore()
	} else {
		var err error
		db, err = postgres.Open(ctx, config.DatabaseURL)
		if err != nil {
			return nil, err
		}
		backend, rooms = db, db
	}
	accounts := auth.WithJWT(backend, config.JWTSecret)
	mux := http.NewServeMux()

	httpapi.Register(mux, accounts, rooms)
	httpapi.RegisterGiftCatalog(mux, accounts, gifts)
	admin.Register(mux, accounts, rooms, config.AdminToken)
	edge.Register(mux, rooms, config.EdgeToken)
	relay.Register(mux, accounts, rooms, config.RelaySecret, config.RelayPublicURL)

	rpcServer := rpc.NewServer()
	rpc.Handle[struct{}, map[string]any](rpcServer, "platform.stats", func(ctx context.Context, _ struct{}) (map[string]any, error) {
		users, err := accounts.Users(ctx)
		if err != nil {
			return nil, err
		}
		roomList, err := rooms.Rooms(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"users": len(users), "rooms": roomList}, nil
	})
	mux.Handle("/internal/rpc/", rpcServer)

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

func (a *App) Run(ctx context.Context) error {
	errors := make(chan error, 1)
	go func() {
		errors <- a.Server.ListenAndServe()
	}()

	select {
	case err := <-errors:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.Server.Shutdown(shutdownContext)
	}
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
