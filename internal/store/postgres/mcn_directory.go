package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/admin"
)

func (s *Store) MCNAgencies(ctx context.Context, q admin.MCNAgencyFilter) ([]admin.MCNAgencyRow, int64, error) {
	const relation = `FROM mcn_agency_directory a WHERE NOT a.disabled
		AND ($1::text IS NULL OR strpos(lower(a.name),lower($1))>0)
		AND ($2::text IS NULL OR strpos(lower(a.super_name),lower($2))>0)`
	const columns = `a.id,a.name,a.super_name,a.email,a.sharing_template_id,
		(SELECT t.name FROM mcn_template_directory t WHERE t.template_id=a.sharing_template_id AND t.agency_id IN(0,a.id)
		 AND NOT t.disabled ORDER BY t.agency_id DESC,t.id LIMIT 1),
		(SELECT count(*) FROM backoffice_mcn_streamers m WHERE m.mcn_id=a.id AND m.status IN(5,6) AND NOT m.superseded),
		((SELECT count(*) FROM backoffice_system_accounts p WHERE p.area='mcn' AND p.scope=a.id::text AND NOT p.deleted)+
		 (SELECT count(*) FROM backoffice_system_bootstrap p WHERE p.area='mcn' AND p.scope=a.id::text))`
	return readBackofficePage(ctx, s, q.PageRequest, columns, relation, "a.id DESC", []any{q.Name, q.SuperName},
		func(row pgx.CollectableRow) (admin.MCNAgencyRow, error) {
			var e admin.MCNAgencyRow
			err := row.Scan(&e.ID, &e.Name, &e.SuperName, &e.SuperEmail, &e.TemplateID, &e.TemplateName, &e.Streamers, &e.Staff)
			return e, err
		})
}

func (s *Store) MCNAgencyStreamers(ctx context.Context, q admin.MCNAgencyStreamerQuery) ([]admin.MCNAgencyStreamer, error) {
	rows, err := s.pool.Query(ctx, `SELECT mcn_id,streamer_id,name,email,joined_at,create_time FROM backoffice_mcn_streamers
		WHERE mcn_id=$1 AND NOT superseded AND status IN (5,6) AND ($2::bigint IS NULL OR streamer_id=$2)
		AND ($3::text IS NULL OR starts_with(lower(name),lower($3))) ORDER BY create_time DESC,id DESC`, q.AgencyID, q.StreamerID, q.Name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNAgencyStreamer, error) {
		var item admin.MCNAgencyStreamer
		err := row.Scan(&item.AgencyID, &item.StreamerID, &item.Name, &item.Email, &item.JoinedAt, &item.CreatedAt)
		return item, err
	})
}

func (s *Store) WriteMCNAgency(ctx context.Context, actor admin.Principal, path, action string, q admin.MCNAgencyEdit, hash string) error {
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		if err := lockSystemArea(ctx, tx, admin.MCNArea); err != nil {
			return err
		}
		if action == "delete" {
			return disableMCNAgency(ctx, tx, actor, path, q.ID)
		}
		if q.TemplateID != nil {
			var exists bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_template_directory WHERE template_id=$1 AND agency_id IN (0,$2) AND NOT disabled)`,
				*q.TemplateID, q.ID).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				return admin.ErrBackofficeNotFound
			}
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_agency_directory WHERE lower(name)=lower($1) AND id<>$2 AND NOT disabled)`,
			q.Name, q.ID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return admin.ErrSystemConflict
		}
		if action == "add" {
			if err := tx.QueryRow(ctx, `INSERT INTO mcn_agency_directory_changes(sharing_template_id,name,super_name,email,agency_level)
				VALUES($1,$2,$3,$4,$5) RETURNING id`, q.TemplateID, q.Name, q.SuperName, q.SuperEmail, q.AgencyLevel).Scan(&q.ID); err != nil {
				return err
			}
			if err := createMCNAgencyAdministrator(ctx, tx, q, hash); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `INSERT INTO mcn_agency_directory_changes(id,sharing_template_id,name,super_name,email,agency_level,ratio,disabled,created_at)
				SELECT id,$2,$3,super_name,email,agency_level,ratio,false,COALESCE(created_at,statement_timestamp()) FROM mcn_agency_directory
				WHERE id=$1 AND NOT disabled ON CONFLICT(id) DO UPDATE SET sharing_template_id=excluded.sharing_template_id,name=excluded.name`,
				q.ID, q.TemplateID, q.Name)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return admin.ErrBackofficeNotFound
			}
		}
		return auditMCNStreamer(ctx, tx, actor, path, "agency_"+action, q.ID)
	})
}

func createMCNAgencyAdministrator(ctx context.Context, tx pgx.Tx, q admin.MCNAgencyEdit, hash string) error {
	var id int64
	if err := tx.QueryRow(ctx, `SELECT nextval('backoffice_system_id_seq')`).Scan(&id); err != nil {
		return err
	}
	scope := strconv.FormatInt(q.ID, 10)
	if err := checkSystemBootstrapCollision(ctx, tx, admin.MCNArea, scope, id, q.SuperEmail); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO backoffice_system_accounts
		(area,scope,id,display_name,email,account_type,agency_level,administrator,password_hash) VALUES('mcn',$1,$2,$3,$4,1,$5,TRUE,$6)`,
		scope, id, q.SuperName, q.SuperEmail, q.AgencyLevel, hash)
	var inputErr *pgconn.PgError
	if errors.As(err, &inputErr) && inputErr.Code == "23505" {
		return admin.ErrSystemConflict
	}
	return err
}

func disableMCNAgency(ctx context.Context, tx pgx.Tx, actor admin.Principal, path string, agency int64) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_mcn_streamers WHERE mcn_id=$1 AND status IN(5,6) AND NOT superseded)
		OR EXISTS(SELECT 1 FROM anchor_profiles WHERE agency_id=$1)`, agency).Scan(&active); err != nil {
		return err
	}
	if active {
		return admin.ErrSystemConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO mcn_agency_directory_changes(id,sharing_template_id,name,super_name,email,agency_level,ratio,disabled,created_at)
		SELECT id,sharing_template_id,name,super_name,email,agency_level,ratio,true,COALESCE(created_at,statement_timestamp()) FROM mcn_agency_directory
		WHERE id=$1 AND NOT disabled ON CONFLICT(id) DO UPDATE SET disabled=true`, agency)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE backoffice_mcn_streamers SET invite_hash=NULL,invite_until=NULL WHERE mcn_id=$1`, agency); err != nil {
		return err
	}
	scope := strconv.FormatInt(agency, 10)
	if _, err := tx.Exec(ctx, `UPDATE backoffice_system_accounts SET disabled=TRUE,credential_version=gen_random_uuid()
		WHERE area='mcn' AND scope=$1 AND NOT deleted`, scope); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM backoffice_sessions s WHERE s.area='mcn' AND EXISTS (
		SELECT 1 FROM backoffice_system_accounts p WHERE p.area=s.area AND p.id=s.principal_id AND p.scope=$1
		UNION ALL SELECT 1 FROM backoffice_system_bootstrap p WHERE p.area=s.area AND p.id=s.principal_id AND p.scope=$1)`, scope); err != nil {
		return err
	}
	return auditMCNStreamer(ctx, tx, actor, path, "agency_delete", agency)
}

func (s *Store) MCNTemplates(ctx context.Context) ([]admin.MCNTemplate, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (template_id) template_id,name FROM mcn_template_directory
		WHERE NOT disabled ORDER BY template_id,id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNTemplate, error) {
		var x admin.MCNTemplate
		err := row.Scan(&x.ID, &x.Name)
		return x, err
	})
}

func (s *Store) MCNTemplateTiers(ctx context.Context, q admin.MCNTemplateQuery) ([]admin.MCNTemplateTier, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,lower_bound,upper_bound,ratio::text FROM mcn_template_directory
		WHERE agency_id=$1 AND template_id=$2 AND NOT disabled ORDER BY lower_bound,id`, q.AgencyID, q.TemplateID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.MCNTemplateTier, error) {
		var x admin.MCNTemplateTier
		err := row.Scan(&x.ID, &x.Name, &x.Min, &x.Max, &x.Ratio)
		return x, err
	})
}

func (s *Store) WriteMCNTemplate(ctx context.Context, actor admin.Principal, path string, q admin.MCNTemplateEdit, remove bool) error {
	return s.mcnChange(ctx, func(tx pgx.Tx) error {
		if remove {
			return disableMCNTemplate(ctx, tx, actor, path, *q.ID)
		}
		var group int64
		err := tx.QueryRow(ctx, `SELECT template_id FROM mcn_template_directory WHERE agency_id=$1 AND NOT disabled
			AND ($2::bigint IS NOT NULL AND (template_id=$2 OR id=$2) OR $2 IS NULL AND lower(name)=lower($3))
			ORDER BY template_id,id LIMIT 1`, q.AgencyID, q.ID, q.Name).Scan(&group)
		if errors.Is(err, pgx.ErrNoRows) && q.ID == nil {
			err = tx.QueryRow(ctx, `SELECT nextval('mcn_directory_id_seq')`).Scan(&group)
		} else if errors.Is(err, pgx.ErrNoRows) {
			return admin.ErrBackofficeNotFound
		}
		if err != nil {
			return err
		}
		var nameTaken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mcn_template_directory WHERE agency_id=$1 AND template_id<>$2
			AND lower(name)=lower($3) AND NOT disabled)`, q.AgencyID, group, q.Name).Scan(&nameTaken); err != nil {
			return err
		}
		if nameTaken {
			return admin.ErrSystemConflict
		}
		if _, err := tx.Exec(ctx, mcnDisableTemplate+`WHERE template_id=$1 AND agency_id=$2 AND NOT disabled
			ON CONFLICT(id) DO UPDATE SET disabled=true`, group, q.AgencyID); err != nil {
			return err
		}
		for _, tier := range q.Data {
			if _, err := tx.Exec(ctx, `INSERT INTO mcn_template_changes(template_id,agency_id,name,lower_bound,upper_bound,ratio)
				VALUES($1,$2,$3,$4,$5,$6)`, group, q.AgencyID, q.Name, int64(tier[0]), int64(tier[1]), int64(tier[2])); err != nil {
				return err
			}
		}
		return auditMCNStreamer(ctx, tx, actor, path, "template_save", group)
	})
}

const mcnDisableTemplate = `INSERT INTO mcn_template_changes(id,template_id,agency_id,name,lower_bound,upper_bound,ratio,disabled)
	SELECT id,template_id,agency_id,COALESCE(name,''),lower_bound,upper_bound,ratio,true FROM mcn_template_directory `

func disableMCNTemplate(ctx context.Context, tx pgx.Tx, actor admin.Principal, path string, id int64) error {
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mcn_agency_directory a JOIN mcn_template_directory t ON a.sharing_template_id=t.template_id
		WHERE NOT a.disabled AND t.agency_id IN(0,a.id) AND NOT t.disabled AND
		(t.template_id=$1 OR t.id=$1 AND NOT EXISTS(SELECT 1 FROM mcn_template_directory replacement
		WHERE replacement.template_id=t.template_id AND replacement.agency_id=t.agency_id AND replacement.id<>t.id AND NOT replacement.disabled)))`,
		id).Scan(&used); err != nil {
		return err
	}
	if used {
		return admin.ErrSystemConflict
	}
	tag, err := tx.Exec(ctx, mcnDisableTemplate+`WHERE (id=$1 OR template_id=$1) AND NOT disabled ON CONFLICT(id) DO UPDATE SET disabled=true`, id)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return auditMCNStreamer(ctx, tx, actor, path, "template_delete", id)
}
