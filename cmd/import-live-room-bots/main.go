package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"

	"open_live_platform/internal/livebot"
	"open_live_platform/internal/store/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv("DATABASE_URL"), os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dsn string, input io.Reader, output io.Writer) error {
	if dsn == "" {
		return errors.New("set DATABASE_URL and pipe the source room bot JSONL into stdin")
	}
	entries, err := readPool(input)
	if err != nil {
		return err
	}
	target, err := postgres.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer target.Shutdown()
	result, err := target.ImportLegacyRoomBotPool(ctx, entries)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "imported=%d existing=%d missing_accounts=%d idle=%d inuse=%d\n",
		result.Imported, result.Existing, result.Missing, result.Idle, result.InUse)
	return err
}

func readPool(input io.Reader) ([]livebot.LegacyPoolEntry, error) {
	var entries []livebot.LegacyPoolEntry
	rows := bufio.NewScanner(input)
	for line := 1; rows.Scan(); line++ {
		data := bytes.TrimSpace(rows.Bytes())
		if len(data) == 0 {
			continue
		}
		var entry livebot.LegacyPoolEntry
		if json.Unmarshal(data, &entry) != nil || entry.Validate() != nil {
			return nil, fmt.Errorf("line %d: invalid room bot pool record", line)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
