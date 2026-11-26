package rtc

import "testing"

func TestSignerConfig(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	signer, err := NewSigner(key, key, "")
	if err != nil || signer.expires != 12*60*60 {
		t.Fatalf("default expiry = %d, error = %v", signer.expires, err)
	}
	for _, cfg := range [][3]string{{key, "", ""}, {"", key, ""}, {key, "invalid", ""}, {key, key, "0"}, {key, key, "-1"}} {
		if _, err := NewSigner(cfg[0], cfg[1], cfg[2]); err == nil {
			t.Error("accepted invalid RTC configuration")
		}
	}
	const userID int64 = 9007199254740993
	token, err := (Signer{}).ForRoom(userID, userID, 200000001, true)
	if err != nil || token.UID != "9007199254740993" || token.Channel != token.UID || token.NID != "200000001" || token.Token != "" {
		t.Fatal("disabled signer failed to preserve the original 64-bit identity")
	}
}
