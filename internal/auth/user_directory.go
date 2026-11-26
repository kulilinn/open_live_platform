package auth

import (
	"context"
	"crypto/subtle"
	"errors"
)

const TrustedUserService = "innow"

var ErrDirectoryUserNotFound = errors.New("user not found")

type UserDirectory struct {
	Accounts AccountStore
	Token    string
}

func (s UserDirectory) Authorized(service, authorization string) bool {
	return service == TrustedUserService && s.Token != "" &&
		subtle.ConstantTimeCompare([]byte(authorization), []byte("Bearer "+s.Token)) == 1
}

func (s UserDirectory) Get(ctx context.Context, id int64) (DirectoryUser, error) {
	user, found, err := s.Accounts.Find(ctx, id)
	if err != nil {
		return DirectoryUser{}, err
	}
	if !found {
		return DirectoryUser{}, ErrDirectoryUserNotFound
	}
	return DirectoryView(user), nil
}

// DirectoryUser restricts the profile fields shared with trusted partner services.
type DirectoryUser struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	SearchName  string `json:"search_name"`
	Language    string `json:"language"`
	Photo       string `json:"photo"`
	Banner      string `json:"banner"`
	IsAnchor    int    `json:"is_anchor"`
	Status      int    `json:"status"`
}

func DirectoryView(user User) DirectoryUser {
	profile := PublicProfile(user)
	anchor := 0
	if profile.IsAnchor {
		anchor = 1
	}
	return DirectoryUser{ID: user.ID, Email: user.Email, Name: profile.Name, DisplayName: user.Account, SearchName: profile.SearchName,
		Language: user.Language, Photo: profile.Photo, Banner: profile.Banner, IsAnchor: anchor, Status: user.Moderation.Status().Type}
}
