package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/store/postgres"
)

func main() {
	appName := flag.String("app", "", "Rust APP_NAME associated with the Redis keys")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: import-live-sessions -app NAME < redis-sessions.jsonl")
		fmt.Fprintln(flag.CommandLine.Output(), "Set DATABASE_URL to the target PostgreSQL database.")
		flag.PrintDefaults()
	}
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv("DATABASE_URL"), *appName, os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dsn, appName string, input io.Reader, output io.Writer) error {
	if dsn == "" || appName == "" {
		return errors.New("DATABASE_URL and -app are required")
	}
	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer store.Shutdown()
	counts := make(map[postgres.LegacyJWTImportResult]int)
	err = importSessions(ctx, appName, input, store, counts)
	_, reportErr := fmt.Fprintf(output, "imported=%d expired=%d account_mismatch=%d existing_session=%d restricted=%d\n",
		counts[postgres.LegacyJWTImported], counts[postgres.LegacyJWTExpired],
		counts[postgres.LegacyJWTAccountMismatch], counts[postgres.LegacyJWTExistingSession], counts[postgres.LegacyJWTRestricted])
	return errors.Join(err, reportErr)
}

func importSessions(ctx context.Context, appName string, input io.Reader, store *postgres.Store, counts map[postgres.LegacyJWTImportResult]int) error {
	scanner := bufio.NewScanner(input)
	for line := 1; scanner.Scan(); line++ {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		var record auth.LegacySessionRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("line %d: invalid session JSON", line)
		}
		session, err := auth.ParseLegacyJWTSession(record, appName)
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		result, err := store.ImportLegacyJWTSession(ctx, session)
		if err != nil {
			return fmt.Errorf("line %d: import session: %w", line, err)
		}
		counts[result]++
	}
	return scanner.Err()
}
