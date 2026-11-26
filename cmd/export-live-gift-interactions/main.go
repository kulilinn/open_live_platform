package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/gift"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("LIVEMEDIA_MYSQL_DSN")
	if dsn == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN")
	}
	config, err := mysql.ParseDSN(dsn)
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

	var catalog gift.Interactions
	catalog.Questions, err = readRows(ctx, snapshot, `SELECT id,subject,target,answer_one,answer_two,answer_three,answer_four,
		answer_five,answer_six,answer_seven,answer_eight,answer_nine,answer_ten,del_flag,create_by,create_time,update_by,update_time
		FROM gift_game_questions WHERE del_flag=0 ORDER BY id`, scanQuestion)
	if err != nil {
		return fmt.Errorf("read gift_game_questions: %w", err)
	}
	catalog.Promotions, err = readRows(ctx, snapshot, `SELECT id,product_id,name,COALESCE(description,''),start_date,end_date
		FROM product_promotions WHERE del_flag=0 ORDER BY id`, func(r *sql.Rows) (gift.Promotion, error) {
		var p gift.Promotion
		err := r.Scan(&p.ID, &p.GiftID, &p.Name, &p.Description, &p.Starts, &p.Ends)
		return p, err
	})
	if err != nil {
		return fmt.Errorf("read product_promotions: %w", err)
	}
	catalog.Limits, err = readRows(ctx, snapshot, `SELECT id,product_id,promotion_id,limit_start_date,limit_end_date,
		discount_price,discount_income,discount_type,discount_value,max_purchase_per_user,max_purchase_platform,listing_status
		FROM product_limits WHERE del_flag=0 ORDER BY id`, func(r *sql.Rows) (gift.Limit, error) {
		var l gift.Limit
		err := r.Scan(&l.ID, &l.GiftID, &l.PromotionID, &l.Starts, &l.Ends, &l.Price, &l.Income, &l.DiscountType, &l.Discount, &l.PerUser, &l.Platform, &l.Listing)
		return l, err
	})
	if err != nil {
		return fmt.Errorf("read product_limits: %w", err)
	}
	catalog.Boxes, err = readRows(ctx, snapshot, `SELECT id,name,gift_ids FROM gift_box WHERE del_flag=0 ORDER BY id`, func(r *sql.Rows) (gift.Box, error) {
		var b gift.Box
		err := r.Scan(&b.ID, &b.Name, &b.LimitIDs)
		return b, err
	})
	if err != nil {
		return fmt.Errorf("read gift_box: %w", err)
	}
	catalog.QuickGifts, err = readRows(ctx, snapshot, `SELECT gift_id,COALESCE(limit_id,0),COALESCE(promotion_id,0),gift_sort FROM quick_gift_config
		WHERE del_flag=0 ORDER BY gift_id`, func(r *sql.Rows) (gift.QuickGift, error) {
		var entry gift.QuickGift
		err := r.Scan(&entry.ID, &entry.LimitID, &entry.PromotionID, &entry.Sort)
		return entry, err
	})
	if err != nil {
		return fmt.Errorf("read quick_gift_config: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(catalog)
}

func scanQuestion(row *sql.Rows) (gift.Question, error) {
	var q gift.Question
	err := row.Scan(&q.ID, &q.Subject, &q.Target, &q.AnswerOne, &q.AnswerTwo, &q.AnswerThree, &q.AnswerFour, &q.AnswerFive,
		&q.AnswerSix, &q.AnswerSeven, &q.AnswerEight, &q.AnswerNine, &q.AnswerTen, &q.Deleted, &q.CreatedBy, &q.CreatedAt,
		&q.UpdatedBy, &q.UpdatedAt)
	return q, err
}

func readRows[T any](ctx context.Context, tx *sql.Tx, query string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
