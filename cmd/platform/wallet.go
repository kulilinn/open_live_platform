package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"open_live_platform/internal/app"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/wallet"
)

func runCreatePass(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: platform create_pass <user_id> <email>")
	}
	owner, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || owner < 1 {
		return errors.New("invalid Wallet pass user ID")
	}
	platform := app.ConfigFromEnv()
	if platform.DatabaseURL == "" {
		return errors.New("set DATABASE_URL to issue Wallet passes")
	}
	config, err := wallet.ConfigFromEnv()
	if err != nil {
		return err
	}
	index, err := postgres.Open(ctx, platform.DatabaseURL)
	if err != nil {
		return err
	}
	defer index.Shutdown()
	link, err := wallet.Issue(ctx, config, index, index, platform.WalletPasses.Storage, platform.JWTSecret, owner, args[1])
	if err != nil {
		return err
	}
	fmt.Println(link)
	return nil
}
