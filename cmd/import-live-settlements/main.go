package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/gift"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if sourceDSN == "" || targetDSN == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN and DATABASE_URL")
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
	lines := func(yield func(gift.EarningsLine, error) bool) {
		rows, err := source.QueryContext(ctx, `SELECT id,anchor_id,user_id,settle_type,settle_ld,subject,create_time
			FROM settle_orders ORDER BY id`)
		if err != nil {
			yield(gift.EarningsLine{}, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var line gift.EarningsLine
			err := rows.Scan(&line.ID, &line.AnchorID, &line.GiverID, &line.Type, &line.Points, &line.Description, &line.CreatedAt)
			if !yield(line, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(gift.EarningsLine{}, err)
		}
	}
	count, err := target.ImportLegacySettlements(ctx, iter.Seq2[gift.EarningsLine, error](lines))
	if err != nil {
		return fmt.Errorf("import MySQL settle_orders: %w", err)
	}
	fmt.Printf("settlements_created=%d\n", count)
	return nil
}
