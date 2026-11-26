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
	targetDSN := os.Getenv("DATABASE_URL")
	if targetDSN == "" {
		return errors.New("set DATABASE_URL")
	}
	appSource, err := readSource(ctx, "LIVEMEDIA_MYSQL_DSN")
	if err != nil {
		return err
	}
	defer appSource.Close()
	mcnSource, err := readSource(ctx, "LIVEMEDIA_MCN_MYSQL_DSN")
	if err != nil {
		return err
	}
	defer mcnSource.Close()
	appTx, err := appSource.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer appTx.Rollback()
	contracts, err := readContracts(ctx, mcnSource)
	if err != nil {
		return err
	}
	target, err := postgres.Open(ctx, targetDSN)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	var imported, unverified int
	for _, contract := range contracts {
		items, err := readProofs(ctx, appTx, contract)
		if err != nil {
			return fmt.Errorf("read MCN proofs for %d: %w", contract.Streamer, err)
		}
		if err := target.ImportLegacyMCNContract(ctx, contract, items); err != nil {
			return fmt.Errorf("import MCN contract %d: %w", contract.ID, err)
		}
		for _, item := range items {
			if item.Pending && item.ProviderID == nil {
				unverified++
			}
		}
		imported++
	}
	fmt.Printf("imported_contracts=%d pending_without_receipts=%d\n", imported, unverified)
	return nil
}

func readSource(ctx context.Context, variable string) (*sql.DB, error) {
	dsn := os.Getenv(variable)
	if dsn == "" {
		return nil, fmt.Errorf("set %s", variable)
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", variable)
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", variable)
	}
	db := sql.OpenDB(connector)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open MySQL source %s: %w", variable, err)
	}
	return db, nil
}

func readContracts(ctx context.Context, source *sql.DB) ([]postgres.LegacyMCNContract, error) {
	rows, err := source.QueryContext(ctx, `SELECT s.id,s.mcn_id,s.streamer_id,NULLIF(s.manager_id,0),u.name,s.email,s.name,
		s.ratio,s.manager_ratio,s.agency_ratio,s.create_time,s.pass_time
		FROM mcn_live_streamer s LEFT JOIN mcn_user u ON u.id=s.manager_id
		WHERE s.status=6 AND s.del_flag=0 AND s.pass_time IS NOT NULL ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []postgres.LegacyMCNContract
	for rows.Next() {
		var item postgres.LegacyMCNContract
		err := rows.Scan(&item.ID, &item.Agency, &item.Streamer, &item.Manager, &item.ManagerName, &item.Email, &item.Name,
			&item.Ratio, &item.ManagerRatio, &item.AgencyRatio, &item.CreatedAt, &item.JoinedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func readProofs(ctx context.Context, source *sql.Tx, contract postgres.LegacyMCNContract) ([]postgres.LegacyMCNSettlementProof, error) {
	var agency int64
	if err := source.QueryRowContext(ctx, `SELECT COALESCE(mcn_id,0) FROM anchor_users WHERE id=? AND state=1`, contract.Streamer).Scan(&agency); err != nil {
		return nil, err
	}
	if agency != contract.Agency {
		return nil, errors.New("MCN affiliation differs between source databases")
	}
	rows, err := source.QueryContext(ctx, `SELECT s.id,p.id,p.no,p.total_point,s.settle_ld,s.bad_debt_ld,s.status,s.create_time
		FROM settle_orders s LEFT JOIN point_transactions p ON p.id=s.point_transaction_id AND BINARY p.no=BINARY s.point_transaction_no
			AND p.user_id=s.user_id AND p.anchor_id=s.anchor_id AND p.income_or_expenses=-1 AND p.no<>''
			AND p.total_point>0 AND p.total_point>=s.expend_point
		WHERE s.anchor_id=? AND (s.status=1 OR s.status=2) AND s.create_time>=? ORDER BY s.id`,
		contract.Streamer, contract.JoinedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []postgres.LegacyMCNSettlementProof
	for rows.Next() {
		var row postgres.LegacyMCNSettlementProof
		var state int
		err := rows.Scan(&row.ID, &row.ProviderID, &row.ProviderNo, &row.ProviderPoints, &row.GrossLD, &row.BadDebtLD, &state, &row.At)
		if err != nil {
			return nil, err
		}
		row.Pending = state == 1
		result = append(result, row)
	}
	return result, rows.Err()
}
