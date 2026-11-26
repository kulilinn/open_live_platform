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
	"open_live_platform/internal/anchor"
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
	snapshot, err := source.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("open MySQL read snapshot: %w", err)
	}
	defer snapshot.Rollback()
	db, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer db.Shutdown()
	err = db.ImportLegacyChannels(ctx,
		rows(ctx, snapshot, `SELECT id,channel,create_at FROM user WHERE COALESCE(channel,'')<>''`,
			func(r *sql.Rows) (anchor.ChannelUser, error) {
				var item anchor.ChannelUser
				err := r.Scan(&item.ID, &item.Channel, &item.Joined)
				return item, err
			}),
		rows(ctx, snapshot, `SELECT p.id,u.id,-CAST(p.pt AS SIGNED),p.time FROM user_pt_log p
			JOIN user u ON BINARY CAST(u.id AS CHAR)=BINARY p.uid WHERE COALESCE(u.channel,'')<>'' AND p.pt<0 AND p.change_type='spend'`,
			func(r *sql.Rows) (anchor.ChannelSpend, error) {
				var item anchor.ChannelSpend
				err := r.Scan(&item.ID, &item.UserID, &item.Points, &item.Created)
				return item, err
			}),
		rows(ctx, snapshot, `SELECT channel,revenue_ratio FROM promotion WHERE channel<>''`,
			func(r *sql.Rows) (anchor.ChannelRate, error) {
				var item anchor.ChannelRate
				err := r.Scan(&item.Channel, &item.Ratio)
				return item, err
			}))
	if err != nil {
		return fmt.Errorf("import channel reporting snapshot: %w", err)
	}
	fmt.Println("anchor channel report snapshot imported")
	return nil
}

func rows[T any](ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scan(rows)
			if !yield(item, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}
