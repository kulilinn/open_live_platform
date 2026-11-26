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

type UserProfilePatch struct {
	DisplayName *string `json:"displayName"`
	SearchName  *string `json:"searchName"`
	Photo       *string `json:"img"`
	Banner      *string `json:"banner"`
	Trans       *bool   `json:"trans"`
	Language    *string `json:"transLang"`
}

func (change UserProfilePatch) IdentityChanged() bool {
	return change.DisplayName != nil || change.SearchName != nil || change.Photo != nil || change.Banner != nil
}

func (change UserProfilePatch) Apply(user User) User {
	profile := PublicProfile(user)
	if change.DisplayName != nil {
		user.Account = *change.DisplayName
	}
	if change.SearchName != nil {
		profile.SearchName = *change.SearchName
	}
	if change.Photo != nil {
		profile.Photo = *change.Photo
	}
	if change.Banner != nil {
		profile.Banner = *change.Banner
	}
	user.Profile = &profile
	return user
}

func (s *Store) UpdateAnchorProfile(ctx context.Context, id int64, display, search, photo, banner string, trans *bool, lang *string) error {
	return s.UpdateUserProfile(ctx, id, UserProfilePatch{DisplayName: &display, SearchName: &search, Photo: &photo, Banner: &banner, Trans: trans, Language: lang})
}

func (s *Store) UpdateUserProfile(ctx context.Context, id int64, change UserProfilePatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var target string
	for email, login := range s.users {
		if login.user.ID == id {
			target = email
		} else if change.SearchName != nil && strings.EqualFold(PublicProfile(login.user).SearchName, *change.SearchName) {
			return ErrSearchNameExists
		}
	}
	if target == "" {
		return ErrInvalidCredentials
	}
	login := s.users[target]
	if change.IdentityChanged() {
		login.user = change.Apply(login.user)
	}
	s.users[target] = login
	if change.Trans != nil {
		s.translationEnabled[id] = *change.Trans
	}
	if change.Language != nil {
		s.translationLanguages[id] = *change.Language
	}
	return nil
}
