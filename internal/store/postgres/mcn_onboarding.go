package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/url"
	"strconv"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/anchor"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/date"
)

var _ admin.MCNOnboardingStore = (*Store)(nil)

type mcnApplication struct {
	ID, Agency, Streamer int64
	Status               int
}

func (s *Store) mcnChange(ctx context.Context, change func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(167884962,61)`); err != nil {
			return err
		}
		return change(tx)
	})
}

func (s *Store) SetMCNApplication(ctx context.Context, actor admin.Principal, path, action, inviteBase string,
	edit admin.MCNEdit, managerName *string, status int) error {
	if actor.Area != admin.MCNArea || actor.AgencyID <= 0 || (action != "add" && action != "edit" && !actor.Admin) {
		return admin.ErrBackofficeForbidden
	}
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		var disabled bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_agency_directory WHERE id=$1 AND disabled)`, actor.AgencyID).Scan(&disabled); err != nil {
			return err
		}
		if disabled {
			return admin.ErrBackofficeForbidden
		}
		if action == "add" {
			return applyMCNStreamer(ctx, tx, actor, path, edit, managerName, inviteBase)
		}
		if edit.ID == nil {
			return admin.ErrBackofficeNotFound
		}
		record, err := findMCNApplication(ctx, tx, actor, *edit.ID)
		if err != nil {
			return err
		}
		if edit.StreamerID != nil && *edit.StreamerID != record.Streamer {
			return admin.ErrBackofficeNotFound
		}
		switch action {
		case "edit":
			err = editMCNStreamer(ctx, tx, record, edit, managerName)
		case "add_confirm":
			err = editMCNStreamer(ctx, tx, record, edit, managerName)
			if err == nil {
				err = inviteMCNStreamer(ctx, tx, record, actor.Name(), inviteBase)
			}
		case "update_status":
			if status == admin.MCNCheckPassed {
				err = inviteMCNStreamer(ctx, tx, record, actor.Name(), inviteBase)
			} else if status == admin.MCNCheckReject {
				err = rejectMCNStreamer(ctx, tx, record)
			} else {
				err = admin.ErrMCNState
			}
		case "account_new":
			if record.Status == admin.MCNJoinFinish {
				return nil
			}
			err = enableMCNStreamer(ctx, tx, record)
			if err == nil {
				err = addMCNMembershipMessage(ctx, tx, record.ID, 2)
			}
		default:
			err = admin.ErrMCNState
		}
		if err != nil {
			return err
		}
		return auditMCNStreamer(ctx, tx, actor, path, action, record.ID)
	})
}

func findMCNApplication(ctx context.Context, tx pgx.Tx, actor admin.Principal, id int64) (mcnApplication, error) {
	var result mcnApplication
	err := tx.QueryRow(ctx, `SELECT id,mcn_id,streamer_id,status FROM backoffice_mcn_streamers
		WHERE id=$1 AND mcn_id=$2 AND ($3 OR manager_id=$4) AND NOT superseded FOR UPDATE`,
		id, actor.AgencyID, actor.Admin, actor.ID).Scan(&result.ID, &result.Agency, &result.Streamer, &result.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = admin.ErrBackofficeNotFound
	}
	return result, err
}

func applyMCNStreamer(ctx context.Context, tx pgx.Tx, actor admin.Principal, path string,
	edit admin.MCNEdit, managerName *string, inviteBase string) error {
	if edit.Email == nil {
		return admin.ErrBackofficeNotFound
	}
	email := auth.NormalizeEmail(*edit.Email)
	var streamer int64
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email=$1
		AND COALESCE((profile->>'IsAnchor')::boolean,FALSE)`, email).Scan(&streamer)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.ErrBackofficeNotFound
	}
	if err != nil {
		return err
	}
	var committed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_mcn_streamers
		WHERE streamer_id=$1 AND NOT superseded AND status IN($2,$3))`,
		streamer, admin.MCNJoinWait, admin.MCNJoinFinish).Scan(&committed); err != nil {
		return err
	}
	if committed {
		return admin.ErrMCNState
	}
	record := mcnApplication{ID: date.NewChannel(), Agency: actor.AgencyID, Streamer: streamer, Status: admin.MCNCheckWait}
	editRatio, managerRatio, agencyRatio := mcnRatios(edit)
	err = tx.QueryRow(ctx, `INSERT INTO backoffice_mcn_streamers AS prior
		(id,mcn_id,streamer_id,manager_id,manager_name,email,name,ratio,manager_ratio,agency_ratio,status,create_time)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8::text::numeric,$9::text::numeric,$10::text::numeric,$11,statement_timestamp())
		ON CONFLICT(mcn_id,streamer_id) DO UPDATE SET manager_id=EXCLUDED.manager_id,manager_name=EXCLUDED.manager_name,
			name=EXCLUDED.name,email=EXCLUDED.email,ratio=EXCLUDED.ratio,manager_ratio=EXCLUDED.manager_ratio,
			agency_ratio=EXCLUDED.agency_ratio,status=EXCLUDED.status,create_time=EXCLUDED.create_time,superseded=FALSE,
			invite_hash=NULL,invite_until=NULL,joined_at=NULL
		WHERE prior.superseded OR prior.status IN ($12,$13)
		RETURNING id`, record.ID, record.Agency, record.Streamer, edit.ManagerID, managerName, email, edit.Name,
		editRatio, managerRatio, agencyRatio, record.Status, admin.MCNCheckReject, admin.MCNNoRenewal).Scan(&record.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.ErrMCNState
	}
	if err != nil {
		return err
	}
	if !actor.Manager {
		if err := inviteMCNStreamer(ctx, tx, record, actor.Name(), inviteBase); err != nil {
			return err
		}
	}
	return auditMCNStreamer(ctx, tx, actor, path, "add", record.ID)
}

func mcnRatios(edit admin.MCNEdit) (string, string, string) {
	manager, agency := "0", "0"
	if edit.ManagerRatio != nil {
		manager = string(*edit.ManagerRatio)
	}
	if edit.AgencyRatio != nil {
		agency = string(*edit.AgencyRatio)
	}
	return string(edit.Ratio), manager, agency
}

func editMCNStreamer(ctx context.Context, tx pgx.Tx, record mcnApplication, edit admin.MCNEdit, managerName *string) error {
	if record.Status == admin.MCNNoRenewal {
		return admin.ErrMCNState
	}
	editRatio, managerRatio, agencyRatio := mcnRatios(edit)
	_, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET name=$2,manager_id=$3,manager_name=$4,
		ratio=$5::text::numeric,manager_ratio=$6::text::numeric,agency_ratio=$7::text::numeric WHERE id=$1`,
		record.ID, edit.Name, edit.ManagerID, managerName, editRatio, managerRatio, agencyRatio)
	return err
}

func inviteMCNStreamer(ctx context.Context, tx pgx.Tx, record mcnApplication, agencyName, inviteBase string) error {
	if record.Status != admin.MCNCheckWait {
		return admin.ErrMCNState
	}
	if inviteBase == "" {
		return admin.ErrMCNInviteUnavailable
	}
	token := rand.Text() + rand.Text()
	hash := sha256.Sum256([]byte(token))
	agency, streamer := strconv.FormatInt(record.Agency, 10), strconv.FormatInt(record.Streamer, 10)
	link, err := url.JoinPath(inviteBase, agency, streamer, token)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET status=$2,invite_hash=$3,
		invite_until=statement_timestamp()+interval '7 days' WHERE id=$1`,
		record.ID, admin.MCNJoinNormal, hash[:]); err != nil {
		return err
	}
	payload := map[string]string{"mcn_id": agency, "streamer_id": streamer, "invite_name": agencyName, "agree_uri": link}
	noticeID := date.NewChannel()
	_, err = tx.Exec(ctx, `INSERT INTO notices(id,notice_target,notice_type,receive_id,content,biz_id,create_time)
		VALUES($1,1,33,$2,$3,$4,statement_timestamp())`, noticeID, record.Streamer, payload, record.ID)
	if err != nil {
		return err
	}
	subject := "LiveMedia agency invitation"
	body := "You have been invited to join a LiveMedia agency.\r\n\r\nReview and confirm the invitation within 7 days:\r\n" + link
	_, err = tx.Exec(ctx, `INSERT INTO mcn_invite_mail(id,membership_id,invite_hash,subject,body) VALUES($1,$2,$3,$4,$5)`,
		noticeID, record.ID, hash[:], subject, body)
	return err
}

func rejectMCNStreamer(ctx context.Context, tx pgx.Tx, record mcnApplication) error {
	if record.Status != admin.MCNCheckWait && record.Status != admin.MCNJoinNormal {
		return admin.ErrMCNState
	}
	_, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET status=$2,invite_hash=NULL,invite_until=NULL WHERE id=$1`,
		record.ID, admin.MCNCheckReject)
	return err
}

func enableMCNStreamer(ctx context.Context, tx pgx.Tx, record mcnApplication) error {
	if record.Status != admin.MCNJoinWait {
		return admin.ErrMCNState
	}
	profile, _, err := anchorForUpdate(ctx, tx, record.Streamer, false)
	if errors.Is(err, anchor.ErrNotFound) || (err == nil && profile.State != anchor.Approved) {
		return admin.ErrBackofficeNotFound
	}
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE anchor_profiles SET agency_id=$2 WHERE user_id=$1 AND agency_id IN (0,$2)`,
		record.Streamer, record.Agency)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return admin.ErrMCNState
	}
	_, err = tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET status=$2,joined_at=statement_timestamp() WHERE id=$1`,
		record.ID, admin.MCNJoinFinish)
	return err
}

func auditMCNStreamer(ctx context.Context, tx pgx.Tx, actor admin.Principal, path, action string, id int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.Area, actor.Scope(), actor.ID, actor.Name(), path,
		strconv.FormatInt(id, 10), "mcn_"+action)
	return err
}

func (s *Store) ConfirmMCNInvitation(ctx context.Context, agency, streamer int64, hash [sha256.Size]byte) error {
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		var record int64
		var expected []byte
		err := tx.QueryRow(ctx, `SELECT id,invite_hash FROM backoffice_mcn_streamers
			WHERE mcn_id=$1 AND streamer_id=$2 AND status=$3 AND NOT superseded AND invite_until>statement_timestamp()
			AND NOT EXISTS(SELECT 1 FROM mcn_agency_directory WHERE id=$1 AND disabled) FOR UPDATE`,
			agency, streamer, admin.MCNJoinNormal).Scan(&record, &expected)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && subtle.ConstantTimeCompare(expected, hash[:]) != 1 {
			return admin.ErrBackofficeForbidden
		}
		if err != nil {
			return err
		}
		var unavailable bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_mcn_streamers
			WHERE streamer_id=$1 AND mcn_id<>$2 AND NOT superseded AND status IN($3,$4))`,
			streamer, agency, admin.MCNJoinWait, admin.MCNJoinFinish).Scan(&unavailable); err != nil {
			return err
		}
		if unavailable {
			return admin.ErrMCNState
		}
		if _, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET status=$2,invite_hash=NULL,invite_until=NULL WHERE id=$1`,
			record, admin.MCNJoinWait); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET superseded=TRUE,invite_hash=NULL,invite_until=NULL
			WHERE streamer_id=$1 AND mcn_id<>$2 AND status IN($3,$4)`, streamer, agency, admin.MCNCheckWait, admin.MCNJoinNormal)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO backoffice_logs(area,tenant,operator_id,operator_name,path,target,action)
			SELECT 'mcn',$2,u.id,u.account,'/api/mcn/streamer/join',$3,'mcn_consent' FROM users u WHERE u.id=$1`,
			streamer, strconv.FormatInt(agency, 10), strconv.FormatInt(record, 10))
		if err != nil {
			return err
		}
		return addMCNMembershipMessage(ctx, tx, record, 1)
	})
}

func addMCNMembershipMessage(ctx context.Context, tx pgx.Tx, membership int64, ty int) error {
	_, err := tx.Exec(ctx, `INSERT INTO mcn_messages(agency_id,manager_id,user_id,ty,params)
		SELECT mcn_id,manager_id,CASE WHEN $2=1 THEN streamer_id ELSE id END,$2,jsonb_build_object('name',name)
		FROM backoffice_mcn_streamers WHERE id=$1`, membership, ty)
	return err
}
