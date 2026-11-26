package auth

import (
	"context"
	"errors"
	"strings"
)

var ErrSearchNameExists = errors.New("search name is already in use")

// Profile holds the public identity fields on LiveMedia's user record.
type Profile struct {
	Name       string
	Gender     int
	Photo      string
	Verified   bool
	Popularity int
	UserType   string
	NSFW       *bool
	IsAnchor   bool
	ToyInfo    string
	Banner     string
	SearchName string
	GroupRoom  string
}

func PublicProfile(user User) Profile {
	if user.Profile != nil {
		return *user.Profile
	}
	return Profile{Name: user.Account, SearchName: user.Account, ToyInfo: "{}", UserType: "normal"}
}

func (s *Store) UpdateAnchorProfile(ctx context.Context, id int64, display, search, photo, banner string, trans *bool, lang *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var target string
	for email, login := range s.users {
		if login.user.ID == id {
			target = email
		} else if strings.EqualFold(PublicProfile(login.user).SearchName, search) {
			return ErrSearchNameExists
		}
	}
	if target == "" {
		return ErrInvalidCredentials
	}
	login := s.users[target]
	profile := PublicProfile(login.user)
	profile.SearchName, profile.Photo, profile.Banner = search, photo, banner
	login.user.Account, login.user.Profile = display, &profile
	s.users[target] = login
	if trans != nil {
		s.translationEnabled[id] = *trans
	}
	if lang != nil {
		s.translationLanguages[id] = *lang
	}
	return nil
}
