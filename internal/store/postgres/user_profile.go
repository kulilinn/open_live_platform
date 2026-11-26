package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/posts"
)

func (s *Store) UpdateUserProfile(ctx context.Context, id int64, change auth.UserProfilePatch) error {
	if !change.IdentityChanged() {
		return s.SetTranslationSettings(ctx, id, change.Trans, change.Language)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if change.SearchName != nil {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,44)`); err != nil {
				return err
			}
		}
		var user auth.User
		err := tx.QueryRow(ctx, `SELECT account,profile FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&user.Account, &user.Profile)
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.ErrInvalidCredentials
		}
		if err != nil {
			return err
		}
		if change.SearchName != nil && auth.PublicProfile(user).SearchName != *change.SearchName {
			var used bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id<>$1 AND lower(COALESCE(profile->>'SearchName',account))=lower($2))`,
				id, *change.SearchName).Scan(&used); err != nil {
				return err
			}
			if used {
				return auth.ErrSearchNameExists
			}
		}
		user = change.Apply(user)
		if _, err := tx.Exec(ctx, `UPDATE users SET account=$2,profile=$3,trans_enabled=COALESCE($4,trans_enabled),trans_lang=COALESCE($5,trans_lang)
			WHERE id=$1`, id, user.Account, user.Profile, change.Trans, change.Language); err != nil {
			return err
		}
		return bindStaticReviews(ctx, tx, id, id, posts.ReviewAvatar, posts.ReviewDisplayName, posts.ReviewSearchName)
	})
}
