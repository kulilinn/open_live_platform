package app_test

import (
	"errors"
	"iter"
	"testing"
	"time"

	"open_live_platform/internal/auth"
	"open_live_platform/internal/store/postgres"
)

func TestPostgresImportLiveMediaAccounts(t *testing.T) {
	config, _ := postgresTestDatabase(t)
	ctx := t.Context()
	db, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Shutdown()
	// Rust's own password-verifier fixture, api/src/auth.rs.
	const phc = "$argon2id$v=19$m=19456,t=2,p=1$m2q/aRQ4nPk0jnUOVXAKIw$tmnbwHvtGHyoSsVq1KvkG2dQGM6SOEPL1QY/Xf18NEo"
	const id = int64(910000000000000001)
	source := postgres.LegacyAccount{ID: id, Email: "Viewer@Example.org", LoginEmail: "viewer@example.org",
		Account: "視聴者", Language: "ja", TranslationLanguage: "en", Password: phc}
	batch := func(rows ...postgres.LegacyAccount) iter.Seq2[postgres.LegacyAccount, error] {
		return func(yield func(postgres.LegacyAccount, error) bool) {
			for _, row := range rows {
				if !yield(row, nil) {
					return
				}
			}
		}
	}
	banned := postgres.LegacyAccount{ID: id + 1, Email: "banned@example.org", LoginEmail: "banned@example.org", Password: phc,
		Status: auth.AccountBanned, LimitUntil: -1, ViolationID: 7}
	violations := func(yield func(postgres.LegacyViolationRecord, error) bool) {
		yield(postgres.LegacyViolationRecord{ID: 7, UserID: banned.ID, Reason: "test-spam,test-policy", LimitDuration: -1,
			CreatedAt: time.Now(), CreatedBy: "Test reviewer", Type: 1}, nil)
	}
	counts, err := db.ImportLegacyAccountSnapshot(ctx, batch(source, banned, postgres.LegacyAccount{ID: id + 2}), violations)
	if err != nil || counts != (postgres.AccountImportCounts{Imported: 3, Restricted: 1, WithoutEmail: 1}) {
		t.Fatalf("import counts = %+v, error = %v", counts, err)
	}
	if _, found, err := db.Find(ctx, id+2); !found || err != nil {
		t.Fatalf("email-less account missing: %t, %v", found, err)
	}
	if _, err := db.SignIn(ctx, "", "12345678"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("empty email allowed password login: %v", err)
	}
	const jwtSecret = "source-no-email-jwt-secret"
	refresh, claims, err := auth.NewJWTService(jwtSecret).IssueRefresh(id+2, "")
	if err != nil {
		t.Fatal(err)
	}
	active := auth.LegacyJWTSession{UserID: id + 2, FamilyID: claims.JTI, ExpiresAt: time.Now().Add(time.Hour)}
	if result, err := db.ImportLegacyJWTSession(ctx, active); result != postgres.LegacyJWTImported || err != nil {
		t.Fatalf("import email-less Redis session = %s, %v", result, err)
	}
	if session, ok, err := auth.WithJWT(db, jwtSecret).Refresh(ctx, refresh); err != nil || !ok || session.User.ID != id+2 || session.User.Email != "" {
		t.Fatalf("email-less legacy refresh = %t, user ID = %d, email = %q, error = %v", ok, session.User.ID, session.User.Email, err)
	}
	var penalty auth.BannedError
	if _, err := db.SignIn(ctx, banned.Email, "12345678"); !errors.As(err, &penalty) || penalty.Moderation.Reason != "test-spam,test-policy" {
		t.Fatalf("banned account login = %v, moderation = %+v", err, penalty)
	}
	if _, err := db.SignIn(ctx, banned.Email, "wrong password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("incorrect credentials returned moderation details: %v", err)
	}
	user, err := db.SignIn(ctx, source.Email, "12345678")
	if err != nil || user.User.ID != id || user.User.Account != source.Account {
		t.Fatalf("old credentials: ID = %d, error = %v", user.User.ID, err)
	}
	if err := db.ChangePassword(ctx, id, "12345678", "new-password"); err != nil {
		t.Fatal(err)
	}
	if counts, err := db.ImportLegacyAccounts(ctx, batch(source)); err != nil || counts.Existing != 1 {
		t.Fatalf("repeat import = %+v, %v", counts, err)
	}
	if _, err := db.VerifyPassword(ctx, source.Email, "12345678"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password after reimport: %v", err)
	}
	if _, err := db.VerifyPassword(ctx, source.Email, "new-password"); err != nil {
		t.Fatalf("new password after reimport: %v", err)
	}
	next, err := db.SignUp(ctx, "next@example.org", "pw", "Next")
	if err != nil || next.User.ID != id+3 {
		t.Fatalf("next signup ID %d, error %v", next.User.ID, err)
	}
	fresh := postgres.LegacyAccount{ID: id + 5, Email: "fresh@example.org"}
	conflict := postgres.LegacyAccount{ID: id + 6, Email: next.User.Email}
	if _, err := db.ImportLegacyAccounts(ctx, batch(fresh, conflict)); err == nil {
		t.Fatal("expected native account conflict")
	}
	if _, ok, err := db.Find(ctx, fresh.ID); ok || err != nil {
		t.Fatalf("rolled back row present = %t, error = %v", ok, err)
	}
}
