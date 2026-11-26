// Command import-live-catalog copies public LiveMedia reference data from MySQL to PostgreSQL.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-sql-driver/mysql"
	"log"
	"open_live_platform/internal/catalog"
	"open_live_platform/internal/store/postgres"
	"os"
	"os/signal"
	"strconv"
	"time"
)

var sections = []struct{ name, query string }{
	{"pwa", "SELECT p.id,p.pwa_id,p.game_url,p.works,p.image,p.weight,a.img AS icon FROM pwa_game p JOIN anchor_users a ON a.id=p.id ORDER BY p.id"},
	{"apps", "SELECT app_id,app_key,app_name,url,callback_url,banner,`desc`,installs,popularity,release_time,state,mobile_client_url FROM apps WHERE state>1 ORDER BY app_id"},
	{"countries", "SELECT code,name,capital,region,currency_code,currency_name,currency_symbol,language_code,language_name,flag,NULL AS _version FROM countries ORDER BY code"},
	{"versions", "SELECT name,platform,curr_version,content_zh,content_ja,content_en,ios_url,android_url,update_time,del_flag,NULL AS _version FROM update_version ORDER BY name"},
	{"marquee", "SELECT id,name,is_jump,jump_url,content,display_order,status,is_deleted FROM marquee ORDER BY display_order,id"},
	{"broadcast_configs", "SELECT id,name,format,jump_type,jump_target,jump_background,jump_name,background FROM broadcast_configs ORDER BY id"},
	{"common_settings", "SELECT id,content FROM common_settings WHERE id IN ('black_ip','white_user','black_user','withdrawal_settings') ORDER BY id"},
	{"bad_words", "SELECT id,name,description,for_country,for_science,url,status FROM bad_words ORDER BY id"},
	{"classify", "SELECT id,pid,name,language,display_order,create_by,create_by_name,create_time,update_by,update_by_name,update_time FROM help_classify ORDER BY display_order,id"},
	{"questions", "SELECT id,first_classify_id,second_classify_id,is_hot,question,language,status,display_order,create_by,create_by_name,create_time,update_by,update_by_name,update_time FROM help_question ORDER BY is_hot DESC,display_order,id"},
	{"answers", "SELECT id,answer,language FROM help_answer ORDER BY id,language"},
}

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
	cfg, err := mysql.ParseDSN(sourceDSN)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	cfg.ParseTime, cfg.Loc = true, time.UTC
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return errors.New("invalid LIVEMEDIA_MYSQL_DSN")
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	data := make(catalog.Snapshot)
	for _, section := range sections {
		rows, err := tx.QueryContext(ctx, section.query)
		if err != nil {
			return fmt.Errorf("read %s: %w", section.name, err)
		}
		data[section.name], err = readRows(rows)
		if err != nil {
			return fmt.Errorf("read %s: %w", section.name, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	dest, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return err
	}
	defer dest.Shutdown()
	if err := dest.ReplacePublicCatalog(ctx, data); err != nil {
		return err
	}
	for _, section := range sections {
		fmt.Printf("%s=%d\n", section.name, len(data[section.name]))
	}
	return nil
}
func readRows(rows *sql.Rows) ([]catalog.Row, error) {
	defer rows.Close()
	cols, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(cols))
	args := make([]any, len(cols))
	for i := range values {
		args[i] = &values[i]
	}
	result := []catalog.Row{}
	for rows.Next() {
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		row := catalog.Row{}
		for i, col := range cols {
			v := values[i]
			if bytes, ok := v.([]byte); ok {
				switch col.DatabaseTypeName() {
				case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT", "UNSIGNED BIGINT":
					n, err := strconv.ParseInt(string(bytes), 10, 64)
					if err != nil {
						return nil, err
					}
					v = n
				default:
					v = string(bytes)
				}
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			row[col.Name()] = raw
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
