package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
)

type TrustedClients map[string]trustedClient

type trustedClient struct {
	tokenHash [32]byte
	redirects []string
}

func ReadTrustedClients(path string) (TrustedClients, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		ClientID  string   `json:"client_id"`
		TokenHash string   `json:"token_hash"`
		Redirects []string `json:"redirect_uris"`
	}
	if err := json.Unmarshal(data, &entries); err != nil || len(entries) == 0 {
		return nil, errors.New("invalid OAuth client configuration")
	}
	clients := make(TrustedClients, len(entries))
	for i, entry := range entries {
		digest, err := hex.DecodeString(entry.TokenHash)
		if err != nil || len(digest) != sha256.Size || strings.TrimSpace(entry.ClientID) != entry.ClientID ||
			entry.ClientID == "" || len(entry.ClientID) > 128 || len(entry.Redirects) == 0 {
			return nil, fmt.Errorf("invalid OAuth client entry %d", i+1)
		}
		if _, exists := clients[entry.ClientID]; exists {
			return nil, fmt.Errorf("duplicate OAuth client at entry %d", i+1)
		}
		for _, raw := range entry.Redirects {
			if !validTrustedRedirect(raw) {
				return nil, fmt.Errorf("invalid OAuth redirect at entry %d", i+1)
			}
		}
		clients[entry.ClientID] = trustedClient{tokenHash: [32]byte(digest), redirects: slices.Compact(entry.Redirects)}
	}
	return clients, nil
}

func validTrustedRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return false
	}
	params, err := url.ParseQuery(u.RawQuery)
	return err == nil && !params.Has("code") && !params.Has("user_id") && !params.Has("state")
}

func (c TrustedClients) Redirect(clientID, raw string) (string, bool) {
	client, ok := c[clientID]
	if !ok {
		return "", false
	}
	if raw == "" && len(client.redirects) == 1 {
		return client.redirects[0], true
	}
	return raw, slices.Contains(client.redirects, raw)
}

func (c TrustedClients) Authorized(clientID, authorization string) bool {
	client, ok := c[clientID]
	token, prefixed := strings.CutPrefix(authorization, "Bearer ")
	hash := sha256.Sum256([]byte(token))
	return ok && prefixed && token != "" && subtle.ConstantTimeCompare(hash[:], client.tokenHash[:]) == 1
}
