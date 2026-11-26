package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/collectible"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

type legacyBinding struct {
	userID  int64
	tokenID uint32
	wallet  string
	slots   map[int8]int32
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	cfg := collectible.ConfigFromEnv()
	if sourceDSN == "" || targetDSN == "" || cfg.RPC == "" || cfg.Contract == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN, DATABASE_URL, WEB3_HTTP and WEB3_CONTRACT_ADDRESS")
	}
	config, err := mysql.ParseDSN(sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	source := sql.OpenDB(connector)
	defer source.Close()
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	service, err := collectible.New(collectible.Config{RPC: cfg.RPC, Contract: cfg.Contract}, target)
	if err != nil {
		return err
	}
	bindings, err := readBindings(ctx, source)
	if err != nil {
		return fmt.Errorf("read MySQL NFT bindings: %w", err)
	}
	var imported, existing, transferred int
	for _, legacy := range bindings {
		binding, err := service.VerifyOwnership(ctx, legacy.tokenID, legacy.wallet)
		if errors.Is(err, collectible.ErrNotOwned) {
			transferred++
			continue
		}
		if err != nil {
			return fmt.Errorf("verify legacy ownership for user %d: %w", legacy.userID, err)
		}
		added, err := target.ImportLegacyNFT(ctx, legacy.userID, binding, legacy.slots)
		if err != nil {
			return fmt.Errorf("import verified legacy NFT for user %d: %w", legacy.userID, err)
		}
		if added {
			imported++
		} else {
			existing++
		}
	}
	fmt.Printf("bindings_created=%d existing_or_missing_user=%d ownership_changed=%d\n", imported, existing, transferred)
	return nil
}

func readBindings(ctx context.Context, source *sql.DB) ([]legacyBinding, error) {
	rows, err := source.QueryContext(ctx, `SELECT c.id,c.token_id,c.account,a.adorn_type,a.adorn_id
		FROM user_creation_card c LEFT JOIN user_equipped_adorn a ON a.user_id=c.id
			AND a.source_type=1 AND (a.expires_at IS NULL OR a.expires_at>UTC_TIMESTAMP())
		WHERE c.status=1 ORDER BY c.id,a.adorn_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []legacyBinding
	for rows.Next() {
		var b legacyBinding
		var slot sql.NullInt16
		var adorn sql.NullInt32
		if err := rows.Scan(&b.userID, &b.tokenID, &b.wallet, &slot, &adorn); err != nil {
			return nil, err
		}
		if len(result) == 0 || result[len(result)-1].userID != b.userID {
			b.slots = make(map[int8]int32)
			result = append(result, b)
		}
		if slot.Valid && slot.Int16 >= 1 && slot.Int16 <= 7 && adorn.Valid && adorn.Int32 > 0 {
			result[len(result)-1].slots[int8(slot.Int16)] = adorn.Int32
		}
	}
	return result, rows.Err()
}
