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
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/store/postgres"
)

// Columns prefixed with ? were introduced after the original MySQL schema.
type sourceTable struct{ name, key, owner, columns string }

var tables = []sourceTable{
	{"room", "room", "uid", "room uid create_at name subject banner online tag description activity avatar chat_id limited offline_info plan gifts live_id area toy_label hot_label teacher_label ticket_label limited_time trans trans_lang violation_record_id"},
	{"room_metrics", "id", "CAST(room AS SIGNED)", "id room audience income new_fans gift_fans start_time end_time fans ?like_quantity"},
	{"room_report", "id", "uid", "id room uid subject description time"},
	{"user_points", "id", "id", "id point total_used_point total_expired_point create_time update_time ?increase_times ?decrease_times ?history_total"},
	{"user_pt_log", "id", "CAST(uid AS SIGNED)", "id uid pt change_type comment time"},
	{"recharges", "id", "user_id", "id no user_id origin_order_id origin_order_no origin_order_type point remain_point expire_time status bad_debt_id bad_debt_no bad_debt_time create_time update_time"},
	{"pay_orders", "id", "user_id", "id user_id amount point payment_method_name status create_time"},
	{"point_transactions", "id", "user_id", "id no user_id anchor_id live_id transaction_type transaction_order_id transaction_order_no subject total_point income_or_expenses total_bad_debt_point create_time update_time"},
	{"gift_production_marketing", "id", "sender_id", "id gift_id sender_id"},
	{"gifts", "id", "id", "id name"},
	{"point_transaction_details", "id", "user_id", "id user_id point_transaction_id recharge_id recharge_no point bad_debt_id bad_debt_no bad_debt_time create_time update_time"},
	{"star_coin_flow_records", "id", "account_id", "id account_id biz_type biz_id amount balance related_record_id status remark create_time"},
}

var mcnTables = []sourceTable{
	{"mcn_info", "id", "id", "id sharing_template_id name super_name email agency_level ratio status create_by create_time update_time"},
	{"mcn_settle_statistic", "id", "mcn_id", "id mcn_id ?live_id manager_id streamer_id ?trace_id ld ?star_coin_total ?live_flow_total ratio ?live_start_time ?live_end_time ?viewer_num ?fans_num ?new_fans_num create_time ?update_time"},
	{"mcn_settle_agency_streamer_statistic", "id", "mcn_id", "id mcn_id manager_id streamer_id ld ratio manager_ratio agency_ratio create_time"},
	{"mcn_settle_platform_statistic", "id", "mcn_id", "id mcn_id ld ratio create_time"},
	{"mcn_sharing_template", "id", "COALESCE(mcn_id,0)", "id template_id mcn_id name min max ratio status create_by create_time update_time"},
	{"mcn_user", "id", "mcn_id", "id mcn_id name"},
	{"mcn_role", "id", "mcn_id", "id mcn_id name manager_name"},
	{"mcn_user_role", "id", "user_id", "id user_id role_id"},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context) error {
	dsn, mcnDSN, targetDSN := os.Getenv("LIVEMEDIA_MYSQL_DSN"), os.Getenv("LIVEMEDIA_MCN_MYSQL_DSN"), os.Getenv("DATABASE_URL")
	if (dsn == "" && mcnDSN == "") || targetDSN == "" {
		return errors.New("set DATABASE_URL and at least one of LIVEMEDIA_MYSQL_DSN or LIVEMEDIA_MCN_MYSQL_DSN")
	}
	db, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return err
	}
	defer db.Shutdown()
	if dsn != "" {
		if err := importSource(ctx, db, dsn, "LIVEMEDIA_MYSQL_DSN", tables); err != nil {
			return err
		}
	}
	if mcnDSN != "" {
		return importSource(ctx, db, mcnDSN, "LIVEMEDIA_MCN_MYSQL_DSN", mcnTables)
	}
	return nil
}

func importSource(ctx context.Context, db *postgres.Store, dsn, variable string, sources []sourceTable) error {
	src, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("invalid %s", variable)
	}
	src.ParseTime, src.Loc = true, time.UTC
	if src.Params == nil {
		src.Params = make(map[string]string)
	}
	src.Params["time_zone"] = "'+00:00'"
	connector, err := mysql.NewConnector(src)
	if err != nil {
		return errors.New("invalid MySQL configuration")
	}
	client := sql.OpenDB(connector)
	defer client.Close()
	tx, err := client.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := db.ImportLegacyHistory(ctx, historyRows(ctx, tx, sources))
	if err != nil {
		return err
	}
	fmt.Printf("read=%d changed=%d rooms=%d broadcasts=%d reports=%d star_balances=%d\n", result.Read, result.Changed, result.Rooms, result.Broadcasts, result.Reports, result.Stars)
	return nil
}

func historyRows(ctx context.Context, tx *sql.Tx, sources []sourceTable) iter.Seq2[postgres.HistoryRow, error] {
	return func(yield func(postgres.HistoryRow, error) bool) {
		for _, t := range sources {
			q, err := historyQuery(ctx, tx, t)
			if err != nil {
				yield(postgres.HistoryRow{}, fmt.Errorf("inspect %s: %w", t.name, err))
				return
			}
			if q == "" {
				fmt.Printf("%s absent in source schema\n", t.name)
				continue
			}
			rows, err := tx.QueryContext(ctx, q)
			if err != nil {
				yield(postgres.HistoryRow{}, fmt.Errorf("read %s: %w", t.name, err))
				return
			}
			for rows.Next() {
				m := postgres.HistoryRow{Source: t.name}
				err := rows.Scan(&m.ID, &m.Owner, &m.Record, &m.Password)
				if !yield(m, err) || err != nil {
					rows.Close()
					return
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				yield(postgres.HistoryRow{}, err)
				return
			}
		}
	}
}
func historyQuery(ctx context.Context, tx *sql.Tx, t sourceTable) (string, error) {
	cols, err := tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`, t.name)
	if err != nil {
		return "", err
	}
	defer cols.Close()
	present := make(map[string]bool)
	for cols.Next() {
		var name string
		if err := cols.Scan(&name); err != nil {
			return "", err
		}
		present[name] = true
	}
	if err := cols.Err(); err != nil {
		return "", err
	}
	if len(present) == 0 {
		return "", nil
	}
	quote := func(n string) string { return "`" + n + "`" }
	pairs := []string{}
	for _, field := range strings.Fields(t.columns) {
		name, optional := strings.CutPrefix(field, "?")
		if !present[name] {
			if optional {
				continue
			}
			return "", fmt.Errorf("missing required column %s", name)
		}
		value := quote(name)
		if t.name == "room" && (name == "plan" || name == "gifts") {
			value = "CAST(" + value + " AS CHAR CHARACTER SET utf8mb4)"
		}
		pairs = append(pairs, "'"+name+"',"+value)
	}
	password := "''"
	if t.name == "room" && present["password"] {
		password = quote("password")
	}
	// All SQL identifiers and expressions in this query come from the static table declaration above.
	return "SELECT CAST(" + quote(t.key) + " AS CHAR)," + t.owner + ",JSON_OBJECT(" + strings.Join(pairs, ",") + ")," + password + " FROM " + quote(t.name) + " ORDER BY " + quote(t.key), nil
}
