package postgres

import (
	"context"
	"errors"
	"net/mail"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/admin"
)

func (s *Store) ApplySystemChange(ctx context.Context, p admin.Principal, change admin.SystemChange) (result any, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSystemArea(ctx, tx, p.Area); err != nil {
			return err
		}
		if p.DatabaseVersion != "" {
			current, err := findSystemPrincipal(ctx, tx, p.Area, p.ID, "", "")
			if errors.Is(err, admin.ErrBackofficeNotFound) {
				return admin.ErrSystemForbidden
			}
			if err != nil {
				return err
			}
			if current.DatabaseVersion != p.DatabaseVersion || current.Scope() != p.Scope() {
				return admin.ErrSystemForbidden
			}
			p = current
		}
		if change.Permission != "" && !p.Can(change.Permission) {
			return admin.ErrSystemForbidden
		}
		var editErr error
		switch change.Resource {
		case "user":
			result, editErr = applySystemAccount(ctx, tx, p, change)
		case "role":
			result, editErr = applySystemRole(ctx, tx, p, change)
		case "menu":
			result, editErr = applySystemMenu(ctx, tx, p, change)
		case "tenant":
			result, editErr = addSystemTenant(ctx, tx, p, change)
		default:
			editErr = admin.ErrSystemInvalid
		}
		return editErr
	})
	return result, systemDBError(err)
}

func systemDBError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23503":
			return admin.ErrSystemConflict
		case "23514":
			return admin.ErrSystemInvalid
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.ErrBackofficeNotFound
	}
	return err
}

func applySystemAccount(ctx context.Context, tx pgx.Tx, p admin.Principal, change admin.SystemChange) (any, error) {
	area, scope, item := p.Area, p.Scope(), change.Account
	if change.Action == "password" {
		if !p.MatchesSystemPassword(change.OldPassword) {
			return nil, admin.ErrSystemForbidden
		}
		_, err := tx.Exec(ctx, `UPDATE backoffice_system_accounts SET password_hash=$4,credential_version=gen_random_uuid()
			WHERE area=$1 AND scope=$2 AND id=$3`, area, scope, p.ID, item.PasswordHash)
		if err != nil {
			return nil, err
		}
		return nil, revokeSystemAccount(ctx, tx, area, p.ID)
	}
	if !canDelegateSystem(p, change) {
		return nil, admin.ErrSystemForbidden
	}
	if change.Action == "delete" {
		ids, err := validSystemIDs(change.IDs, item.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if _, err := manageableSystemUser(ctx, tx, p, id); err != nil {
				return nil, err
			}
			_, err := tx.Exec(ctx, `UPDATE backoffice_system_accounts SET deleted=true,disabled=true,credential_version=gen_random_uuid()
				WHERE area=$1 AND scope=$2 AND id=$3`, area, scope, id)
			if err != nil {
				return nil, err
			}
			if err := revokeSystemAccount(ctx, tx, area, id); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if change.Action == "edit" {
		previous, err := manageableSystemUser(ctx, tx, p, item.ID.Value())
		if err != nil {
			return nil, err
		}
		if item.Username == "" {
			item.Username = previous.Username
		}
		if area == admin.MCNArea && item.Email == "" {
			item.Email = previous.Email
		}
		if item.Type == 0 {
			item.Type = previous.Type
		}
		item.AgencyLevel, item.Disabled = previous.AgencyLevel, previous.Disabled
		if !change.Elevation {
			item.Admin = previous.Admin
		}
	} else if change.Action != "add" || item.PasswordHash == "" {
		return nil, admin.ErrSystemInvalid
	}
	if change.Status != nil {
		if *change.Status != 0 && *change.Status != 1 {
			return nil, admin.ErrSystemInvalid
		}
		item.Disabled = *change.Status == 1
	}
	if !p.Admin && (item.Admin || item.Type == 1 || item.Type == 2 || change.Elevation) {
		return nil, admin.ErrSystemForbidden
	}
	if item.DisplayName == "" {
		item.DisplayName = systemLogin(item, area)
	}
	if len(item.DisplayName) > 150 || len(item.Remark) > 2000 || item.Gender < 0 || item.Gender > 2 ||
		item.Type < 0 || item.Type > 3 || systemLogin(item, area) == "" || len(systemLogin(item, area)) > 320 ||
		area == admin.MCNArea && !systemEmailValid(item.Email) {
		return nil, admin.ErrSystemInvalid
	}
	if err := checkSystemBootstrapCollision(ctx, tx, area, scope, item.ID.Value(), systemLogin(item, area)); err != nil {
		return nil, err
	}
	roleIDs, err := validSystemIDList(item.RoleIDs, 1000)
	if err != nil {
		return nil, err
	}
	menuIDs, err := validSystemIDList(item.UserPrivilege, 1000)
	if err != nil {
		return nil, err
	}
	if area != admin.MCNArea && len(menuIDs) > 0 {
		return nil, admin.ErrSystemInvalid
	}
	for _, id := range roleIDs {
		if err := manageableSystemRole(ctx, tx, p, id, false); err != nil {
			return nil, err
		}
	}
	if err := manageableSystemMenus(ctx, tx, p, menuIDs); err != nil {
		return nil, err
	}
	if change.Action == "add" {
		err = tx.QueryRow(ctx, `INSERT INTO backoffice_system_accounts(area,scope,username,display_name,email,phone,gender,
			account_type,agency_level,administrator,disabled,remark,password_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			RETURNING id,created_at`, area, scope, item.Username, item.DisplayName, item.Email, item.Phone, item.Gender,
			item.Type, item.AgencyLevel, item.Admin, item.Disabled, item.Remark, item.PasswordHash).Scan(&item.ID, &item.Created)
	} else {
		var revoked bool
		err = tx.QueryRow(ctx, `WITH old AS (SELECT id,username,email,account_type,administrator,disabled FROM backoffice_system_accounts
			WHERE area=$1 AND scope=$2 AND id=$3), updated AS (
			UPDATE backoffice_system_accounts AS a SET username=$4,display_name=$5,email=$6,phone=$7,gender=$8,account_type=$9,
				administrator=$10,disabled=$11,remark=$12,password_hash=COALESCE(NULLIF($13,''),a.password_hash),
				credential_version=CASE WHEN (old.username,old.email,old.account_type,old.administrator,old.disabled) IS DISTINCT FROM
					($4::text,$6::text,$9::smallint,$10::boolean,$11::boolean) OR $13<>'' THEN gen_random_uuid() ELSE a.credential_version END
			FROM old WHERE (a.area,a.scope,a.id)=($1,$2,old.id)
			RETURNING a.created_at,(old.username,old.email,old.account_type,old.administrator,old.disabled) IS DISTINCT FROM
					($4::text,$6::text,$9::smallint,$10::boolean,$11::boolean) OR $13<>'' AS revoked)
			SELECT created_at,revoked FROM updated`, area, scope, item.ID.Value(), item.Username, item.DisplayName, item.Email, item.Phone, item.Gender,
			item.Type, item.Admin, item.Disabled, item.Remark, item.PasswordHash).Scan(&item.Created, &revoked)
		if err == nil && revoked {
			err = revokeSystemAccount(ctx, tx, area, item.ID.Value())
		}
	}
	if err != nil {
		return nil, err
	}
	if item.RoleIDs != nil || change.Action == "add" {
		if err := replaceSystemGrants(ctx, tx, area, scope, item.ID.Value(), "members", roleIDs); err != nil {
			return nil, err
		}
	}
	if area == admin.MCNArea && (item.UserPrivilege != nil || change.Action == "add") {
		if err := replaceSystemGrants(ctx, tx, area, scope, item.ID.Value(), "user_menus", menuIDs); err != nil {
			return nil, err
		}
	}
	item.PasswordHash = ""
	return item, nil
}

func canDelegateSystem(p admin.Principal, change admin.SystemChange) bool {
	return change.Permission != "" && p.Can(change.Permission) && (p.Admin || p.Area == admin.OperationArea || p.Manager)
}

func manageableSystemUser(ctx context.Context, tx pgx.Tx, p admin.Principal, id int64) (admin.SystemAccount, error) {
	var item admin.SystemAccount
	if id <= 0 {
		return item, admin.ErrSystemInvalid
	}
	if p.ID == id {
		return item, admin.ErrSystemForbidden
	}
	err := tx.QueryRow(ctx, `SELECT username,email,account_type,agency_level,administrator,disabled
		FROM backoffice_system_accounts WHERE area=$1 AND scope=$2 AND id=$3 AND NOT deleted`, p.Area, p.Scope(), id).Scan(
		&item.Username, &item.Email, &item.Type, &item.AgencyLevel, &item.Admin, &item.Disabled)
	if err != nil {
		return item, err
	}
	if p.Admin {
		return item, nil
	}
	if item.Admin || p.Area == admin.MCNArea && (item.Type == 1 || item.Type == 2) {
		return item, admin.ErrSystemForbidden
	}
	var outside bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM backoffice_system_menus menu WHERE menu.area=$1 AND menu.scope=$2 AND menu.permission<>''
		AND NOT (menu.permission=ANY($4::text[])) AND menu.id IN (
		  SELECT rm.menu_id FROM backoffice_system_members AS m JOIN backoffice_system_role_menus AS rm USING(area,scope,role_id)
		  WHERE m.area=$1 AND m.scope=$2 AND m.account_id=$3
		  UNION SELECT menu_id FROM backoffice_system_user_menus WHERE area=$1 AND scope=$2 AND account_id=$3))`,
		p.Area, p.Scope(), id, systemTextList(p.Permissions)).Scan(&outside)
	if err == nil && outside {
		err = admin.ErrSystemForbidden
	}
	return item, err
}

func revokeSystemAccount(ctx context.Context, tx pgx.Tx, area string, id int64) error {
	_, err := tx.Exec(ctx, "DELETE FROM backoffice_sessions WHERE area=$1 AND principal_id=$2", area, id)
	return err
}

func validSystemIDs(ids []admin.SystemID, fallback admin.SystemID) ([]int64, error) {
	if len(ids) == 0 && fallback > 0 {
		ids = []admin.SystemID{fallback}
	}
	if len(ids) == 0 {
		return nil, admin.ErrSystemInvalid
	}
	return validSystemIDList(ids, 100)
}

func validSystemIDList(list []admin.SystemID, maxCount int) ([]int64, error) {
	if len(list) > maxCount {
		return nil, admin.ErrSystemInvalid
	}
	values := make([]int64, 0, len(list))
	seen := make(map[int64]bool, len(list))
	for _, id := range list {
		if id <= 0 {
			return nil, admin.ErrSystemInvalid
		}
		if !seen[id.Value()] {
			values = append(values, id.Value())
			seen[id.Value()] = true
		}
	}
	return values, nil
}

func manageableSystemRole(ctx context.Context, tx pgx.Tx, p admin.Principal, id int64, changing bool) error {
	var exists, outside, owns bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_system_roles WHERE area=$1 AND scope=$2 AND id=$3),
		EXISTS(SELECT 1 FROM backoffice_system_role_menus rm
			JOIN backoffice_system_menus m ON (m.area,m.scope,m.id)=(rm.area,rm.scope,rm.menu_id)
			WHERE rm.area=$1 AND rm.scope=$2 AND rm.role_id=$3 AND m.permission<>'' AND NOT (m.permission=ANY($4::text[]))),
		EXISTS(SELECT 1 FROM backoffice_system_members WHERE area=$1 AND scope=$2 AND role_id=$3 AND account_id=$5)`,
		p.Area, p.Scope(), id, systemTextList(p.Permissions), p.ID).Scan(&exists, &outside, &owns)
	if err != nil {
		return err
	}
	if !exists {
		return admin.ErrBackofficeNotFound
	}
	if !p.Admin && (outside || changing && owns) {
		return admin.ErrSystemForbidden
	}
	return nil
}

func manageableSystemMenus(ctx context.Context, tx pgx.Tx, p admin.Principal, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	var found int
	var outside bool
	err := tx.QueryRow(ctx, `SELECT count(*),coalesce(bool_or(permission<>'' AND NOT (permission=ANY($4::text[]))),false)
		FROM backoffice_system_menus WHERE area=$1 AND scope=$2 AND id=ANY($3::bigint[])`, p.Area, p.Scope(), ids,
		systemTextList(p.Permissions)).Scan(&found, &outside)
	if err != nil {
		return err
	}
	if found != len(ids) {
		return admin.ErrBackofficeNotFound
	}
	if !p.Admin && outside {
		return admin.ErrSystemForbidden
	}
	return nil
}

func replaceSystemGrants(ctx context.Context, tx pgx.Tx, area, scope string, id int64, relation string, ids []int64) error {
	var table, owner, grant string
	switch relation {
	case "members":
		table, owner, grant = "backoffice_system_members", "account_id", "role_id"
	case "user_menus":
		table, owner, grant = "backoffice_system_user_menus", "account_id", "menu_id"
	case "role_menus":
		table, owner, grant = "backoffice_system_role_menus", "role_id", "menu_id"
	default:
		return admin.ErrSystemInvalid
	}
	// Keeping unchanged rows retains any existing group-leader assignment.
	_, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE area=$1 AND scope=$2 AND "+owner+"=$3 AND NOT ("+grant+"=ANY($4::bigint[]))", area, scope, id, ids)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO "+table+"(area,scope,"+owner+","+grant+") SELECT $1,$2,$3,item FROM unnest($4::bigint[]) item ON CONFLICT DO NOTHING",
		area, scope, id, ids)
	return err
}

func systemEmailValid(value string) bool {
	email, err := mail.ParseAddress(value)
	return err == nil && email.Address == value
}

func applySystemRole(ctx context.Context, tx pgx.Tx, p admin.Principal, change admin.SystemChange) (any, error) {
	if !canDelegateSystem(p, change) {
		return nil, admin.ErrSystemForbidden
	}
	area, scope, role := p.Area, p.Scope(), change.Role
	switch change.Action {
	case "set_group_admin", "clean_group_admin":
		if !p.Admin || area != admin.MCNArea || role.ID <= 0 || change.Member <= 0 {
			return nil, admin.ErrSystemForbidden
		}
		result, err := tx.Exec(ctx, `UPDATE backoffice_system_members m SET group_admin=$5
			WHERE (m.area,m.scope,m.role_id,m.account_id)=($1,$2,$3,$4) AND EXISTS(
			SELECT 1 FROM backoffice_system_accounts a WHERE (a.area,a.scope,a.id)=(m.area,m.scope,m.account_id) AND NOT a.deleted AND NOT a.disabled)`,
			area, scope, role.ID.Value(), change.Member.Value(), change.Action == "set_group_admin")
		if err == nil && result.RowsAffected() != 1 {
			err = admin.ErrBackofficeNotFound
		}
		return nil, err
	case "delete":
		ids, err := validSystemIDs(change.IDs, role.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if err := manageableSystemRole(ctx, tx, p, id, true); err != nil {
				return nil, err
			}
		}
		_, err = tx.Exec(ctx, "DELETE FROM backoffice_system_roles WHERE area=$1 AND scope=$2 AND id=ANY($3::bigint[])", area, scope, ids)
		return nil, err
	case "edit":
		if err := manageableSystemRole(ctx, tx, p, role.ID.Value(), true); err != nil {
			return nil, err
		}
	case "add":
	default:
		return nil, admin.ErrSystemInvalid
	}
	if change.Status != nil {
		role.Status = *change.Status
	}
	if strings.TrimSpace(role.Name) == "" || len(role.Name) > 150 || len(role.Code) > 150 || role.Status < 0 || role.Status > 1 {
		return nil, admin.ErrSystemInvalid
	}
	menus, err := validSystemIDList(role.MenuIDs, 1000)
	if err != nil {
		return nil, err
	}
	if err := manageableSystemMenus(ctx, tx, p, menus); err != nil {
		return nil, err
	}
	if change.Action == "add" {
		err = tx.QueryRow(ctx, `INSERT INTO backoffice_system_roles(area,scope,name,code,sort,status,remark) VALUES($1,$2,$3,$4,$5,$6,$7)
			RETURNING id,created_at`, area, scope, role.Name, role.Code, role.Sort, role.Status, role.Remark).Scan(&role.ID, &role.Created)
	} else {
		err = tx.QueryRow(ctx, `UPDATE backoffice_system_roles SET name=$4,code=$5,sort=$6,status=coalesce($7,status),remark=$8
			WHERE area=$1 AND scope=$2 AND id=$3 RETURNING status,created_at`, area, scope, role.ID.Value(), role.Name, role.Code, role.Sort,
			change.Status, role.Remark).Scan(&role.Status, &role.Created)
	}
	if err != nil {
		return nil, err
	}
	if role.MenuIDs != nil || change.Action == "add" {
		if err := replaceSystemGrants(ctx, tx, area, scope, role.ID.Value(), "role_menus", menus); err != nil {
			return nil, err
		}
	}
	return role, nil
}

var systemPermissionPattern = regexp.MustCompile(`^api(?::[A-Za-z0-9_-]+)+$`)
var systemTenantCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,79}$`)

func applySystemMenu(ctx context.Context, tx pgx.Tx, p admin.Principal, change admin.SystemChange) (any, error) {
	if !p.Admin || change.Permission == "" {
		return nil, admin.ErrSystemForbidden
	}
	area, scope, menu := p.Area, p.Scope(), change.Menu
	if change.Action == "delete" {
		ids, err := validSystemIDs(change.IDs, menu.ID)
		if err != nil {
			return nil, err
		}
		result, err := tx.Exec(ctx, "DELETE FROM backoffice_system_menus WHERE area=$1 AND scope=$2 AND id=ANY($3::bigint[])", area, scope, ids)
		if err == nil && result.RowsAffected() != int64(len(ids)) {
			err = admin.ErrBackofficeNotFound
		}
		return nil, err
	}
	if change.Action != "add" && change.Action != "edit" || strings.TrimSpace(menu.Name) == "" || len(menu.Name) > 150 ||
		len(menu.URL) > 2048 || len(menu.Permission) > 255 || menu.Hidden < 0 || menu.Hidden > 1 ||
		menu.Permission != "" && !systemPermissionPattern.MatchString(menu.Permission) {
		return nil, admin.ErrSystemInvalid
	}
	if menu.Parent > 0 {
		var parentExists, cycle bool
		err := tx.QueryRow(ctx, `WITH RECURSIVE ancestors(id,parent_id) AS (
			SELECT id,parent_id FROM backoffice_system_menus WHERE area=$1 AND scope=$2 AND id=$3
			UNION SELECT m.id,m.parent_id FROM backoffice_system_menus m JOIN ancestors a ON m.id=a.parent_id WHERE m.area=$1 AND m.scope=$2
			) SELECT EXISTS(SELECT 1 FROM ancestors),EXISTS(SELECT 1 FROM ancestors WHERE id=$4)`,
			area, scope, menu.Parent.Value(), menu.ID.Value()).Scan(&parentExists, &cycle)
		if err != nil {
			return nil, err
		}
		if !parentExists || cycle {
			return nil, admin.ErrSystemInvalid
		}
	}
	var err error
	if change.Action == "add" {
		err = tx.QueryRow(ctx, `INSERT INTO backoffice_system_menus(area,scope,parent_id,name,url,menu_type,hidden,target,refresh,permission,icon,sort,remark)
			VALUES($1,$2,NULLIF($3,0),$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id,created_at`, area, scope, menu.Parent.Value(),
			menu.Name, menu.URL, menu.Type, menu.Hidden, menu.Target, menu.Refresh, menu.Permission, menu.Icon, menu.Sort, menu.Remark).Scan(&menu.ID, &menu.Created)
	} else {
		err = tx.QueryRow(ctx, `UPDATE backoffice_system_menus SET parent_id=NULLIF($4,0),name=$5,url=$6,menu_type=$7,
			hidden=$8,permission=$9,icon=$10,sort=$11,remark=$12 WHERE area=$1 AND scope=$2 AND id=$3
			RETURNING target,refresh,created_at`,
			area, scope, menu.ID.Value(), menu.Parent.Value(), menu.Name, menu.URL, menu.Type, menu.Hidden, menu.Permission, menu.Icon, menu.Sort, menu.Remark).
			Scan(&menu.Target, &menu.Refresh, &menu.Created)
	}
	return menu, err
}

func addSystemTenant(ctx context.Context, tx pgx.Tx, p admin.Principal, change admin.SystemChange) (any, error) {
	if !p.Admin || p.Area != admin.OperationArea || p.Tenant != admin.OperationTenant || change.Action != "add" {
		return nil, admin.ErrSystemForbidden
	}
	item, owner := change.Tenant, change.Account
	if !systemTenantCodePattern.MatchString(item.Code) || len(item.Name) > 150 || strings.TrimSpace(item.Name) == "" ||
		(owner.Username == "") != (owner.PasswordHash == "") {
		return nil, admin.ErrSystemInvalid
	}
	err := tx.QueryRow(ctx, `INSERT INTO backoffice_system_tenants(code,name,remark) VALUES($1,$2,$3) RETURNING id,created_at`,
		item.Code, item.Name, item.Remark).Scan(&item.ID, &item.Created)
	if err != nil || owner.Username == "" {
		return item, err
	}
	if err := checkSystemBootstrapCollision(ctx, tx, p.Area, item.Code, 0, owner.Username); err != nil {
		return nil, err
	}
	if owner.DisplayName == "" {
		owner.DisplayName = owner.Username
	}
	_, err = tx.Exec(ctx, `INSERT INTO backoffice_system_accounts(area,scope,username,display_name,email,administrator,password_hash)
		VALUES($1,$2,$3,$4,$5,true,$6)`, p.Area, item.Code, owner.Username, owner.DisplayName, owner.Email, owner.PasswordHash)
	return item, err
}
