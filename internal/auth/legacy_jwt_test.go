package auth

import (
	"encoding/base64"
	"testing"
	"time"
)

func TestParseLegacyJWTSession(t *testing.T) {
	// Fixed bincode 1.x AuthUser fixtures with both optional fields unset and set.
	for _, value := range []string{
		"AZCHdkc2FwETAAAAAAAAAEV4YW1wbGVARXhhbXBsZS5vcmexaN46AAAAAAOQh3ZHNhcBAAAC",
		"AZCHdkc2FwETAAAAAAAAAEV4YW1wbGVARXhhbXBsZS5vcmexaN46AAAAAAOQh3ZHNhcBAbBo3joAAAAAAR4AAAAAAAAAMjAyNS0xMC0wMVQwMTowMjowMy4xMjM0NTY3ODlaAg==",
	} {
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		record := LegacySessionRecord{
			Key:       "{78591199061446657}|auth_user|LiveMedia",
			Value:     data,
			ExpiresAt: time.Unix(1_760_000_000, 123_000_000),
		}
		want := LegacyJWTSession{
			UserID: 78591199061446657, Email: "Example@Example.org",
			FamilyID: 78591199061446659, ExpiresAt: record.ExpiresAt,
		}
		if got, err := ParseLegacyJWTSession(record, "LiveMedia"); err != nil || got != want {
			t.Fatalf("session = %+v, error = %v, want %+v", got, err, want)
		}
		if _, err := ParseLegacyJWTSession(record, "other-app"); err == nil {
			t.Fatal("accepted record for another app")
		}
		wrongKey := record
		wrongKey.Key = "{78591199061446658}|auth_user|LiveMedia"
		if _, err := ParseLegacyJWTSession(wrongKey, "LiveMedia"); err == nil {
			t.Fatal("accepted record for another account")
		}
		missingExpiry := record
		missingExpiry.ExpiresAt = time.Time{}
		if _, err := ParseLegacyJWTSession(missingExpiry, "LiveMedia"); err == nil {
			t.Fatal("accepted record without an absolute expiry")
		}
		for size := range len(data) {
			truncated := record
			truncated.Value = data[:size]
			if _, err := ParseLegacyJWTSession(truncated, "LiveMedia"); err == nil {
				t.Fatalf("accepted truncated record of %d bytes", size)
			}
		}
	}
}
