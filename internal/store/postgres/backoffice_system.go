package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"open_live_platform/internal/admin"
)

var _ admin.SystemStore = (*Store)(nil)

func lockSystemArea(ctx context.Context, tx pgx.Tx, area string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('backoffice-system:' || $1,0))", area)
	return err
}

func checkSystemBootstrapCollision(ctx context.Context, tx pgx.Tx, area, scope string, id int64, login string) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM backoffice_system_bootstrap
		WHERE area=$1 AND (id=$3 OR (lower(login)=lower($4) AND ($1='mcn' OR scope=$2))))`, area, scope, id, login).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return admin.ErrSystemConflict
	}
	return nil
}

func (s *Store) ReserveSystemBootstrap(ctx context.Context, principals admin.Principals) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, area := range []string{admin.OperationArea, admin.MCNArea} {
			if err := lockSystemArea(ctx, tx, area); err != nil {
				return err
			}
			for _, p := range principals {
				if p.Area != area {
					continue
				}
				var collision bool
				err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_system_accounts WHERE area=$1 AND
					(id=$2 OR ($1='mcn' AND lower(email)=lower($4)) OR ($1='admin' AND scope=$3 AND lower(username)=lower($4))))`,
					p.Area, p.ID, p.Scope(), p.Name()).Scan(&collision)
				if err != nil {
					return err
				}
				if collision {
					return fmt.Errorf("bootstrap backoffice principal %s/%d conflicts with a database account", p.Area, p.ID)
				}
				tag, err := tx.Exec(ctx, `INSERT INTO backoffice_system_bootstrap(area,id,scope,login) VALUES($1,$2,$3,$4)
					ON CONFLICT (area,id) DO UPDATE SET login=EXCLUDED.login WHERE
					backoffice_system_bootstrap.scope=EXCLUDED.scope AND lower(backoffice_system_bootstrap.login)=lower(EXCLUDED.login)`,
					p.Area, p.ID, p.Scope(), p.Name())
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 1 {
					return fmt.Errorf("bootstrap backoffice principal %s/%d has a different reserved identity", p.Area, p.ID)
				}
			}
		}
		return nil
	})
}

func (s *Store) FindSystemPrincipal(ctx context.Context, area string, id int64, scope, name string) (p admin.Principal, err error) {
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var resolveErr error
		p, resolveErr = findSystemPrincipal(ctx, tx, area, id, scope, name)
		return resolveErr
	})
	return p, err
}

func findSystemPrincipal(ctx context.Context, tx pgx.Tx, area string, id int64, scope, name string) (admin.Principal, error) {
	var account admin.SystemAccount
	err := tx.QueryRow(ctx, `SELECT scope,id,username,display_name,email,account_type,administrator,password_hash,credential_version::text
		FROM backoffice_system_accounts WHERE area=$1 AND NOT disabled AND NOT deleted AND
		(($2::bigint>0 AND id=$2) OR ($2=0 AND (($1='admin' AND scope=$3 AND lower(username)=lower($4)) OR ($1='mcn' AND lower(email)=lower($4)))))`,
		area, id, scope, name).Scan(&scope, &account.ID, &account.Username, &account.DisplayName, &account.Email,
		&account.Type, &account.Admin, &account.PasswordHash, &account.Credential)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.Principal{}, admin.ErrBackofficeNotFound
	}
	if err != nil {
		return admin.Principal{}, err
	}
	rows, err := tx.Query(ctx, `SELECT CASE WHEN r.code<>'' THEN r.code ELSE r.name END
		FROM backoffice_system_members AS m JOIN backoffice_system_roles AS r USING(area,scope)
		WHERE m.area=$1 AND m.scope=$2 AND m.account_id=$3 AND r.id=m.role_id AND r.status=0 ORDER BY 1`, area, scope, account.ID.Value())
	if err != nil {
		return admin.Principal{}, err
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return admin.Principal{}, err
	}
	rows, err = tx.Query(ctx, `SELECT `+systemMenuColumns+` FROM backoffice_system_menus AS menu
		WHERE menu.area=$1 AND menu.scope=$2 AND ($4 OR menu.id IN (
			SELECT rm.menu_id FROM backoffice_system_members AS m
			JOIN backoffice_system_roles AS r ON (r.area,r.scope,r.id)=(m.area,m.scope,m.role_id) AND r.status=0
			JOIN backoffice_system_role_menus AS rm ON (rm.area,rm.scope,rm.role_id)=(m.area,m.scope,m.role_id)
			WHERE m.area=$1 AND m.scope=$2 AND m.account_id=$3
			UNION SELECT menu_id FROM backoffice_system_user_menus WHERE area=$1 AND scope=$2 AND account_id=$3
		)) ORDER BY menu.sort,menu.id`, area, scope, account.ID.Value(), account.Admin)
	if err != nil {
		return admin.Principal{}, err
	}
	menus, err := pgx.CollectRows(rows, scanSystemMenu)
	if err != nil {
		return admin.Principal{}, err
	}
	return admin.NewDatabasePrincipal(area, scope, account, slices.Compact(roles), menus)
}

const systemMenuColumns = `menu.id,coalesce(menu.parent_id,0),menu.name,menu.url,menu.menu_type,menu.hidden,menu.target,
	menu.refresh,menu.permission,menu.icon,menu.sort,menu.remark,menu.created_at`

func scanSystemMenu(row pgx.CollectableRow) (admin.SystemMenu, error) {
	var menu admin.SystemMenu
	err := row.Scan(&menu.ID, &menu.Parent, &menu.Name, &menu.URL, &menu.Type, &menu.Hidden, &menu.Target,
		&menu.Refresh, &menu.Permission, &menu.Icon, &menu.Sort, &menu.Remark, &menu.Created)
	return menu, err
}

func (s *Store) ListSystemMenus(ctx context.Context, p admin.Principal) ([]admin.SystemMenu, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+systemMenuColumns+` FROM backoffice_system_menus AS menu
		WHERE menu.area=$1 AND menu.scope=$2 AND ($3 OR menu.permission='' OR menu.permission=ANY($4::text[])) ORDER BY menu.sort,menu.id`,
		p.Area, p.Scope(), p.Admin, p.Permissions)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSystemMenu)
}

func (s *Store) ListSystemAccounts(ctx context.Context, p admin.Principal, f admin.SystemFilter) ([]admin.SystemAccount, int64, error) {
	const columns = `a.id,a.username,a.display_name,a.email,a.phone,a.gender,a.account_type,a.agency_level,
		a.administrator,a.disabled,a.remark,a.created_at,coalesce(roles.role_name,''),coalesce(roles.manager_name,''),
		array(SELECT m.role_id FROM backoffice_system_members AS m WHERE (m.area,m.scope,m.account_id)=(a.area,a.scope,a.id) ORDER BY m.role_id),
		array(SELECT m.menu_id FROM backoffice_system_user_menus AS m WHERE (m.area,m.scope,m.account_id)=(a.area,a.scope,a.id) ORDER BY m.menu_id)`
	const relations = `FROM backoffice_system_accounts AS a LEFT JOIN LATERAL (
		SELECT string_agg(DISTINCT r.name,', ' ORDER BY r.name) AS role_name,
			string_agg(DISTINCT leader.display_name,', ' ORDER BY leader.display_name) AS manager_name
		FROM backoffice_system_members AS m
		JOIN backoffice_system_roles AS r ON (r.area,r.scope,r.id)=(m.area,m.scope,m.role_id)
		LEFT JOIN backoffice_system_members AS g ON (g.area,g.scope,g.role_id)=(m.area,m.scope,m.role_id) AND g.group_admin
		LEFT JOIN backoffice_system_accounts AS leader ON (leader.area,leader.scope,leader.id)=(g.area,g.scope,g.account_id) AND NOT leader.disabled AND NOT leader.deleted
		WHERE (m.area,m.scope,m.account_id)=(a.area,a.scope,a.id)
	) AS roles ON TRUE WHERE a.area=$1 AND a.scope=$2 AND NOT a.deleted AND ($3::bigint=0 OR a.id=$3)
		AND ($4 OR a.id=$5 OR (NOT a.administrator AND ($3=0 OR $6)))
		AND ($7::text='' OR strpos(lower(a.username),lower($7))=1)
		AND ($8::text='' OR strpos(lower(a.display_name),lower($8))=1)
		AND ($9::smallint IS NULL OR a.account_type=$9)
		AND ($10::text='' OR strpos(lower(roles.role_name),lower($10))>0)
		AND ($11::text='' OR strpos(lower(roles.manager_name),lower($11))>0)
		AND (NOT $12::boolean OR (a.account_type=2 AND NOT a.disabled))`
	queryRight := "api:system:user:page"
	if p.Area == admin.OperationArea {
		queryRight = "api:operation:system:user:page"
	}
	return readSystemRecords(ctx, s, f, columns, relations, "a.id", []any{p.Area, p.Scope(), f.ID.Value(), p.Admin, p.ID,
		p.Can(queryRight), f.Username, f.Name, f.Type, f.RoleName, f.ManagerName, f.Managers}, scanSystemAccount)
}

func scanSystemAccount(row pgx.CollectableRow) (admin.SystemAccount, error) {
	var a admin.SystemAccount
	var roles, menus []int64
	err := row.Scan(&a.ID, &a.Username, &a.DisplayName, &a.Email, &a.Phone, &a.Gender, &a.Type, &a.AgencyLevel,
		&a.Admin, &a.Disabled, &a.Remark, &a.Created, &a.RoleName, &a.ManagerName, &roles, &menus)
	a.RoleIDs, a.UserPrivilege = systemIDs(roles), systemIDs(menus)
	return a, err
}

func (s *Store) ListSystemRoles(ctx context.Context, p admin.Principal, f admin.SystemFilter) ([]admin.SystemRole, int64, error) {
	const columns = `r.id,r.name,r.code,r.sort,r.status,r.remark,r.created_at,groups.staff,coalesce(groups.manager_name,''),
		array(SELECT rm.menu_id FROM backoffice_system_role_menus AS rm WHERE (rm.area,rm.scope,rm.role_id)=(r.area,r.scope,r.id) ORDER BY rm.menu_id)`
	const relations = `FROM backoffice_system_roles AS r LEFT JOIN LATERAL (
		SELECT count(*) AS staff,string_agg(a.display_name,', ' ORDER BY a.display_name) FILTER(WHERE m.group_admin AND NOT a.disabled) AS manager_name
		FROM backoffice_system_members AS m JOIN backoffice_system_accounts AS a ON (a.area,a.scope,a.id)=(m.area,m.scope,m.account_id) AND NOT a.deleted
		WHERE (m.area,m.scope,m.role_id)=(r.area,r.scope,r.id)
	) AS groups ON TRUE WHERE r.area=$1 AND r.scope=$2 AND ($3::bigint=0 OR r.id=$3)
		AND ($4 OR NOT EXISTS (
			SELECT 1 FROM backoffice_system_role_menus AS rm JOIN backoffice_system_menus AS menu ON (menu.area,menu.scope,menu.id)=(rm.area,rm.scope,rm.menu_id)
			WHERE (rm.area,rm.scope,rm.role_id)=(r.area,r.scope,r.id) AND menu.permission<>'' AND NOT (menu.permission=ANY($5::text[]))
		))
		AND ($6::text='' OR strpos(lower(r.name),lower($6))=1)
		AND ($7::text='' OR strpos(lower(groups.manager_name),lower($7))>0)`
	return readSystemRecords(ctx, s, f, columns, relations, "r.sort,r.id", []any{p.Area, p.Scope(), f.ID.Value(), p.Admin,
		systemTextList(p.Permissions), f.Name, f.ManageName}, scanSystemRole)
}

func scanSystemRole(row pgx.CollectableRow) (admin.SystemRole, error) {
	var role admin.SystemRole
	var menus []int64
	err := row.Scan(&role.ID, &role.Name, &role.Code, &role.Sort, &role.Status, &role.Remark, &role.Created, &role.Staff, &role.ManagerName, &menus)
	role.MenuIDs = systemIDs(menus)
	return role, err
}

func (s *Store) ListSystemGroup(ctx context.Context, p admin.Principal, id int64) ([]admin.SystemGroupMember, error) {
	if id <= 0 {
		return nil, admin.ErrSystemInvalid
	}
	var group bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backoffice_system_roles r WHERE area=$1 AND scope=$2 AND id=$3 AND
		($4 OR EXISTS(SELECT 1 FROM backoffice_system_members m WHERE (m.area,m.scope,m.role_id)=(r.area,r.scope,r.id) AND m.account_id=$5)))`,
		p.Area, p.Scope(), id, p.Admin, p.ID).Scan(&group)
	if err != nil {
		return nil, err
	}
	if !group {
		return nil, admin.ErrBackofficeNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.display_name,a.email,CASE WHEN m.group_admin THEN 1 ELSE 0 END FROM backoffice_system_members m
		JOIN backoffice_system_accounts a ON (a.area,a.scope,a.id)=(m.area,m.scope,m.account_id)
		WHERE m.area=$1 AND m.scope=$2 AND m.role_id=$3 AND NOT a.deleted ORDER BY a.id`, p.Area, p.Scope(), id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (admin.SystemGroupMember, error) {
		var m admin.SystemGroupMember
		err := row.Scan(&m.ID, &m.Name, &m.Email, &m.GroupAdmin)
		return m, err
	})
}

func readSystemRecords[T any](ctx context.Context, s *Store, f admin.SystemFilter, columns, relations, order string, args []any, scan func(pgx.CollectableRow) (T, error)) ([]T, int64, error) {
	if !f.All {
		return readBackofficePage(ctx, s, f.PageRequest, columns, relations, order, args, scan)
	}
	rows, err := s.pool.Query(ctx, "SELECT "+columns+" "+relations+" ORDER BY "+order, args...)
	if err != nil {
		return nil, 0, err
	}
	results, err := pgx.CollectRows(rows, scan)
	return results, int64(len(results)), err
}

func systemIDs(ids []int64) []admin.SystemID {
	out := make([]admin.SystemID, len(ids))
	for i, id := range ids {
		out[i] = admin.SystemID(id)
	}
	return out
}

func systemTextList(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func systemLogin(p admin.SystemAccount, area string) string {
	if area == admin.MCNArea {
		return strings.TrimSpace(p.Email)
	}
	return strings.TrimSpace(p.Username)
}
