package wallet

import (
	"cmp"
	"errors"
	"os"
	"strconv"
	"time"
)

const defaultTokenTTL = 180 * 24 * time.Hour
const DownloadTTL = 24 * time.Hour

type Config struct {
	Certificate, PrivateKey, WWDRCertificate string
	Assets, PassType, TeamID                 string
	LoginURL, WebServiceURL, PublicURL       string
	TokenTTL                                 time.Duration
}

func ConfigFromEnv() (Config, error) {
	ttl := defaultTokenTTL
	if raw := os.Getenv("PASS_TOKEN_EXPIRED"); raw != "" {
		seconds, err := strconv.ParseUint(raw, 10, 63)
		if err != nil || seconds < 1 || seconds > uint64((1<<63-1)/int64(time.Second)) {
			return Config{}, errors.New("PASS_TOKEN_EXPIRED must be a positive duration in seconds")
		}
		ttl = time.Duration(seconds) * time.Second
	}
	return Config{
		Certificate:     cmp.Or(os.Getenv("APPLE_PASS_CERT"), "apple_pass_cer.pem"),
		PrivateKey:      cmp.Or(os.Getenv("APPLE_PASS_KEY"), "apple_pass_key.pem"),
		WWDRCertificate: cmp.Or(os.Getenv("APPLE_PASS_WWDR_CERT"), "AppleWWDRCAG4.cer"),
		Assets:          cmp.Or(os.Getenv("APPLE_PASS_ASSET_DIR"), "data"),
		PassType:        cmp.Or(os.Getenv("APPLE_PASS_TYPE_IDENTIFIER"), "pass.love.rose"),
		TeamID:          cmp.Or(os.Getenv("APPLE_PASS_TEAM_IDENTIFIER"), "2S28B5WQA9"),
		LoginURL:        cmp.Or(os.Getenv("APPLE_PASS_LOGIN_URL"), "https://www.rose.love/"),
		WebServiceURL:   os.Getenv("APPLE_PASS_WEB_SERVICE_URL"),
		PublicURL:       os.Getenv("BASE_URL"),
		TokenTTL:        ttl,
	}, nil
}
