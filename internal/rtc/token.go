package rtc

import (
	"cmp"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/AgoraIO-Community/go-tokenbuilder/rtctokenbuilder2"
)

type Signer struct {
	appID, certificate string
	expires            uint32
}

func NewSigner(appID, certificate, expirySeconds string) (Signer, error) {
	if appID == "" && certificate == "" {
		return Signer{}, nil
	}
	for _, key := range []string{appID, certificate} {
		if data, err := hex.DecodeString(key); err != nil || len(data) != 16 {
			return Signer{}, errors.New("AGORA_APP_ID and AGORA_APP_CERTIFICATE must each be 32 hex digits")
		}
	}
	ttl, err := strconv.ParseUint(cmp.Or(expirySeconds, "43200"), 10, 32)
	if err != nil || ttl == 0 {
		return Signer{}, errors.New("AGORA_TOKEN_EXPIRE must be a positive number of seconds within uint32 range")
	}
	return Signer{appID, certificate, uint32(ttl)}, nil
}

type Token struct {
	AppID    string `json:"appid"`
	Channel  string `json:"channel"`
	UID      string `json:"uid"`
	NID      string `json:"nid"`
	Token    string `json:"token"`
	IsAnchor bool   `json:"isAnchor"`
}

func (s Signer) ForRoom(roomID, userID int64, nid uint32, publisher bool) (Token, error) {
	token := Token{
		AppID: s.appID, Channel: strconv.FormatInt(roomID, 10), UID: strconv.FormatInt(userID, 10),
		NID: strconv.FormatUint(uint64(nid), 10), IsAnchor: publisher,
	}
	if s.appID == "" {
		return token, nil
	}
	role := rtctokenbuilder2.Role(rtctokenbuilder2.RoleSubscriber)
	if publisher {
		role = rtctokenbuilder2.RolePublisher
	}
	signed, err := rtctokenbuilder2.BuildTokenWithUid(s.appID, s.certificate, token.Channel, nid, role, s.expires, s.expires)
	token.Token = signed
	return token, err
}
