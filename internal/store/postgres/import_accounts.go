package postgres

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

type LegacyAccount struct {
	ID                  int64
	Email, LoginEmail   string
	Account, Language   string
	TranslationLanguage string
	TranslationEnabled  bool
	Password            string
	Status              int
	Profile             auth.Profile
}

type AccountImportCounts struct {
	Imported, Existing, Restricted, WithoutEmail int
}

// ImportLegacyAccounts takes a source snapshot ordered by user ID. A retry
// preserves any password or profile changed after the original import.
func (s *Store) ImportLegacyAccounts(ctx context.Context, rows iter.Seq2[LegacyAccount, error]) (AccountImportCounts, error) {
	var counts AccountImportCounts
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize explicit ID imports with signup and sequence adjustment.
		if _, err := tx.Exec(ctx, "LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			return err
		}
		var lastID int64
		for row, err := range rows {
			if err != nil {
				return err
			}
			if row.ID <= lastID {
				return fmt.Errorf("duplicate, unordered or invalid source user ID %d", row.ID)
			}
			lastID = row.ID
			if row.Status != 0 {
				counts.Restricted++
				continue
			}
			email := auth.NormalizeEmail(row.Email)
			if row.LoginEmail != "" && auth.NormalizeEmail(row.LoginEmail) != email {
				return fmt.Errorf("profile and login emails differ for source user %d", row.ID)
			}
			if email == "" {
				counts.WithoutEmail++
				continue
			}
			if row.LoginEmail != "" {
				if _, ok := auth.ParseLegacyPassword(row.Password); !ok {
					return fmt.Errorf("unsupported password hash for source user %d", row.ID)
				}
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO users(id,email,account,language,trans_lang,trans_enabled,password_salt,password_hash,livemedia_imported,profile)
				VALUES ($1,$2,$3,$4,$5,$6,''::bytea,$7,true,$8) ON CONFLICT DO NOTHING`,
				row.ID, email, row.Account, row.Language, row.TranslationLanguage, row.TranslationEnabled, []byte(row.Password), row.Profile)
			if err != nil {
				return fmt.Errorf("import user %d: %w", row.ID, err)
			}
			if tag.RowsAffected() == 1 {
				counts.Imported++
				continue
			}
			var imported bool
			err = tx.QueryRow(ctx, "SELECT livemedia_imported FROM users WHERE id=$1 AND email=$2", row.ID, email).Scan(&imported)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !imported) {
				return fmt.Errorf("target account conflicts with source user %d", row.ID)
			}
			if err != nil {
				return err
			}
			counts.Existing++
		}
		if lastID == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `
			SELECT setval('users_id_seq', highest.id, true)
			FROM (SELECT greatest($1::bigint, max(id)) AS id FROM users) highest, users_id_seq seq
			WHERE highest.id > seq.last_value OR (highest.id = seq.last_value AND NOT seq.is_called)`, lastID)
		return err
	})
	if err != nil {
		return AccountImportCounts{}, err
	}
	return counts, nil
}
