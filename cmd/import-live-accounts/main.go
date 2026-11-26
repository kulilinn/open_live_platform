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

	_ "github.com/go-sql-driver/mysql"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	sourceDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if sourceDSN == "" || targetDSN == "" {
		return errors.New("set LIVEMEDIA_MYSQL_DSN and DATABASE_URL")
	}
	source, err := sql.Open("mysql", sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	defer source.Close()
	rows, err := source.QueryContext(ctx, "SELECT u.id,u.email,u.display_name,COALESCE(u.language,'ja'),"+
		"u.trans_lang,u.trans,u.status,COALESCE(e.email,''),COALESCE(e.password,''),"+
		"COALESCE(u.name,''),COALESCE(u.gender,0),COALESCE(u.photo,''),COALESCE(u.verified,0),COALESCE(u.popularity,0),"+
		"u.user_type,u.nsfw,u.is_anchor,COALESCE(u.toy_info,'{}'),u.banner,u.search_name,u.group_room "+
		"FROM `user` u LEFT JOIN email_login e ON e.uid=u.id ORDER BY u.id")
	if err != nil {
		return fmt.Errorf("read MySQL accounts: %w", err)
	}
	defer rows.Close()
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	counts, err := target.ImportLegacyAccounts(ctx, accounts(rows))
	if err != nil {
		return err
	}
	fmt.Printf("imported=%d existing=%d restricted=%d without_email=%d\n",
		counts.Imported, counts.Existing, counts.Restricted, counts.WithoutEmail)
	return nil
}

func accounts(rows *sql.Rows) iter.Seq2[postgres.LegacyAccount, error] {
	return func(yield func(postgres.LegacyAccount, error) bool) {
		for rows.Next() {
			var a postgres.LegacyAccount
			p := &a.Profile
			err := rows.Scan(&a.ID, &a.Email, &a.Account, &a.Language, &a.TranslationLanguage,
				&a.TranslationEnabled, &a.Status, &a.LoginEmail, &a.Password,
				&p.Name, &p.Gender, &p.Photo, &p.Verified, &p.Popularity, &p.UserType, &p.NSFW, &p.IsAnchor, &p.ToyInfo, &p.Banner, &p.SearchName, &p.GroupRoom)
			if !yield(a, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(postgres.LegacyAccount{}, err)
		}
	}
}
