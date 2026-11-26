package wallet

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/upload"
)

var ErrPassAccount = errors.New("Wallet pass requires the matching account ID and email")

func Issue(ctx context.Context, config Config, account auth.AccountStore, index upload.PassIssuerStore, storage upload.Config,
	secret string, userID int64, email string) (string, error) {
	email = strings.TrimSpace(email)
	if userID < 1 || email == "" || secret == "" || !storage.Ready() || index == nil {
		return "", errors.New("Wallet pass issuance requires JWT_SECRET, S3 configuration, account and index")
	}
	publicURL, err := walletURL(config.PublicURL, false)
	if err != nil {
		return "", errors.New("BASE_URL must be an HTTPS URL for Wallet pass issuance")
	}
	user, found, err := account.Find(ctx, userID)
	if err != nil {
		return "", err
	}
	if !found || user.Email == "" || !strings.EqualFold(user.Email, email) {
		return "", ErrPassAccount
	}
	tokens := auth.NewJWTService(secret)
	now := time.Now().UTC()
	signing, err := openSigner(config, now)
	if err != nil {
		return "", err
	}
	for range 3 {
		token, claims, err := tokens.IssueWalletPass(userID, email, config.TokenTTL)
		if err != nil {
			return "", err
		}
		issued, expiry := time.Unix(claims.Iat, 0).UTC(), time.Unix(claims.Exp, 0).UTC()
		record := upload.PassIssue{ID: claims.JTI, UserID: userID, IssuedAt: issued, ExpiresAt: issued.Add(DownloadTTL), TokenExpiresAt: expiry}
		bundle, err := signing.bundle(userID, token, expiry)
		if err != nil {
			return "", err
		}
		reserved, err := index.ReserveIssuedPass(ctx, record)
		if err != nil {
			return "", err
		}
		if !reserved {
			continue
		}
		if err := storage.PutBytes(ctx, record.Object(), bundle); err != nil {
			return "", err
		}
		if err := index.PublishIssuedPass(ctx, record.ID); err != nil {
			return "", err
		}
		return url.JoinPath(publicURL.String(), "pass", strconv.FormatInt(record.ID, 10))
	}
	return "", errors.New("could not reserve a unique Wallet pass")
}
