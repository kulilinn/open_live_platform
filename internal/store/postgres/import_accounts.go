package postgres

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/auth"
)

type LegacyAccount struct {
	ID                   int64
	Email, LoginEmail    string
	Account, Language    string
	TranslationLanguage  string
	TranslationEnabled   bool
	Password             string
	Status               int
	LimitUntil           int64
	ViolationID          int64
	Profile              auth.Profile
	CreatedAt, LastLogin *time.Time
}

type LegacyViolationRecord struct {
	ID, UserID    int64
	Reason        string
	LimitDuration int64
	CreatedAt     time.Time
	CreatedBy     string
	Type          int
}

type AccountImportCounts struct {
	Imported, Existing, Restricted, WithoutEmail int
}

// ImportLegacyAccounts takes a source snapshot ordered by user ID. A retry
// preserves any password or profile changed after the original import.
func (s *Store) ImportLegacyAccounts(ctx context.Context, rows iter.Seq2[LegacyAccount, error]) (AccountImportCounts, error) {
	return s.ImportLegacyAccountSnapshot(ctx, rows, nil)
}

func (s *Store) ImportLegacyAccountSnapshot(ctx context.Context, rows iter.Seq2[LegacyAccount, error], violations iter.Seq2[LegacyViolationRecord, error]) (AccountImportCounts, error) {
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
			if row.Status < auth.AccountNormal || row.Status > auth.AccountBanned || row.ViolationID < 0 {
				return fmt.Errorf("invalid source moderation for user %d", row.ID)
			}
			email := auth.NormalizeEmail(row.Email)
			loginEmail := auth.NormalizeEmail(row.LoginEmail)
			if loginEmail != "" && email != "" && loginEmail != email {
				return fmt.Errorf("profile and login emails differ for source user %d", row.ID)
			}
			if email == "" {
				email = loginEmail
			}
			if email == "" {
				counts.WithoutEmail++
			}
			if row.Status != auth.AccountNormal {
				counts.Restricted++
			}
			if loginEmail != "" && row.Password != "" {
				if _, ok := auth.ParseLegacyPassword(row.Password); !ok {
					return fmt.Errorf("unsupported password hash for source user %d", row.ID)
				}
			} else {
				row.Password = ""
			}
			tag, err := tx.Exec(ctx, `
				INSERT INTO users(id,email,account,language,trans_lang,trans_enabled,password_salt,password_hash,livemedia_imported,profile,
					account_status,account_limit_until,account_violation_id,legacy_profile_email,created_at)
				VALUES ($1,NULLIF($2,''),$3,$4,$5,$6,''::bytea,$7,true,$8,$9,$10,$11,$12,coalesce($13,now())) ON CONFLICT DO NOTHING`,
				row.ID, email, row.Account, row.Language, row.TranslationLanguage, row.TranslationEnabled, []byte(row.Password), row.Profile,
				row.Status, row.LimitUntil, row.ViolationID, row.Email, row.CreatedAt)
			if err != nil {
				return fmt.Errorf("import user %d: %w", row.ID, err)
			}
			if row.LastLogin != nil {
				if _, err := tx.Exec(ctx, `INSERT INTO user_login_activity(user_id,last_login_at) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET last_login_at=greatest(user_login_activity.last_login_at,excluded.last_login_at)`, row.ID, row.LastLogin); err != nil {
					return err
				}
			}
			if tag.RowsAffected() == 1 {
				counts.Imported++
				continue
			}
			var imported bool
			err = tx.QueryRow(ctx, `UPDATE users SET account_status=$3,account_limit_until=$4,account_violation_id=$5,legacy_profile_email=$6,created_at=coalesce($7,created_at)
				WHERE id=$1 AND email IS NOT DISTINCT FROM NULLIF($2,'') AND livemedia_imported RETURNING livemedia_imported`,
				row.ID, email, row.Status, row.LimitUntil, row.ViolationID, row.Email, row.CreatedAt).Scan(&imported)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !imported) {
				return fmt.Errorf("target account conflicts with source user %d", row.ID)
			}
			if err != nil {
				return err
			}
			if (auth.Moderation{Type: row.Status, Until: row.LimitUntil}).Banned() {
				if err := s.revokeBannedSessions(ctx, tx, row.ID); err != nil {
					return err
				}
			}
			counts.Existing++
		}
		if violations != nil {
			if err := importLegacyViolationRows(ctx, tx, violations); err != nil {
				return err
			}
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

func importLegacyViolationRows(ctx context.Context, tx pgx.Tx, rows iter.Seq2[LegacyViolationRecord, error]) error {
	for row, err := range rows {
		if err != nil {
			return err
		}
		if row.ID < 1 || row.UserID < 1 || row.CreatedAt.IsZero() {
			return errors.New("invalid source account violation record")
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_violation_records(id,user_id,reason,limit_duration,create_time,create_by_name,ty)
			VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(id) DO NOTHING`, row.ID, row.UserID, row.Reason, row.LimitDuration,
			row.CreatedAt, row.CreatedBy, row.Type)
		if err != nil {
			return fmt.Errorf("import account violation %d: %w", row.ID, err)
		}
	}
	return nil
}
