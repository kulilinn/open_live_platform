package postgres

import (
	"context"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/auth"
)

type LegacySystemAccount struct {
	Scope   string
	Account admin.SystemAccount
}

type LegacySystemRole struct {
	Scope string
	Role  admin.SystemRole
}

type LegacySystemMenu struct {
	Scope string
	Menu  admin.SystemMenu
}

type LegacySystemGrant struct {
	Scope         string
	Owner, Target int64
	GroupAdmin    bool
}

type LegacyBackoffice struct {
	Area      string
	Tenants   iter.Seq2[admin.SystemTenant, error]
	Accounts  iter.Seq2[LegacySystemAccount, error]
	Roles     iter.Seq2[LegacySystemRole, error]
	Menus     iter.Seq2[LegacySystemMenu, error]
	Members   iter.Seq2[LegacySystemGrant, error]
	RoleMenus iter.Seq2[LegacySystemGrant, error]
	UserMenus iter.Seq2[LegacySystemGrant, error]
}

type LegacyBackofficeResult struct {
	Tenants             int `json:"tenants"`
	Accounts            int `json:"accounts"`
	Roles               int `json:"roles"`
	Menus               int `json:"menus"`
	Members             int `json:"members"`
	RoleMenus           int `json:"roleMenus"`
	UserMenus           int `json:"userMenus"`
	UnusableCredentials int `json:"unusableCredentials"`
	Skipped             int `json:"skipped"`
	Existing            int `json:"existing"`
}

type backofficeImportKey struct {
	scope string
	id    int64
}

func (s *Store) ImportLegacyBackoffice(ctx context.Context, areas []LegacyBackoffice) (map[string]LegacyBackofficeResult, error) {
	results := make(map[string]LegacyBackofficeResult)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Startup and all management writers take the same locks in this order.
		for _, area := range []string{admin.OperationArea, admin.MCNArea} {
			if err := lockSystemArea(ctx, tx, area); err != nil {
				return err
			}
		}
		for _, realm := range areas {
			if realm.Area != admin.OperationArea && realm.Area != admin.MCNArea {
				return admin.ErrSystemInvalid
			}
			var previous LegacyBackofficeResult
			err := tx.QueryRow(ctx, `SELECT summary FROM backoffice_system_imports WHERE area=$1`, realm.Area).Scan(&previous)
			if err == nil {
				results[realm.Area] = LegacyBackofficeResult{
					Existing: previous.Tenants + previous.Accounts + previous.Roles + previous.Menus +
						previous.Members + previous.RoleMenus + previous.UserMenus,
					Skipped: previous.Skipped, UnusableCredentials: previous.UnusableCredentials}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			result, err := importLegacyBackofficeArea(ctx, tx, realm)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO backoffice_system_imports(area,summary) VALUES($1,$2)`, realm.Area, result)
			if err != nil {
				return err
			}
			results[realm.Area] = result
		}
		return nil
	})
	return results, err
}

func importLegacyBackofficeArea(ctx context.Context, tx pgx.Tx, realm LegacyBackoffice) (LegacyBackofficeResult, error) {
	var result LegacyBackofficeResult
	area := realm.Area
	scopes := map[string]bool{}
	accounts, roles, menus := map[backofficeImportKey]bool{}, map[backofficeImportKey]bool{}, map[backofficeImportKey]bool{}
	if realm.Tenants != nil {
		for row, err := range realm.Tenants {
			if err != nil {
				return result, err
			}
			if row.ID <= 0 || !validLegacySystemScope(area, row.Code) || row.Name == "" {
				result.Skipped++
				continue
			}
			tag, err := tx.Exec(ctx, `INSERT INTO backoffice_system_tenants(id,code,name,remark,created_at)
				VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, row.ID.Value(), row.Code, row.Name, row.Remark, row.Created)
			if err := countLegacyBackofficeRow(tag, err, &result.Tenants, &result.Skipped); err != nil {
				return result, err
			}
		}
	}

	for row, err := range realm.Accounts {
		if err != nil {
			return result, err
		}
		a := row.Account
		if a.ID <= 0 || !validLegacySystemScope(area, row.Scope) || systemLogin(a, area) == "" {
			result.Skipped++
			continue
		}
		scopes[row.Scope] = true
		err = checkSystemBootstrapCollision(ctx, tx, area, row.Scope, a.ID.Value(), systemLogin(a, area))
		if errors.Is(err, admin.ErrSystemConflict) {
			result.Skipped++
			continue
		}
		if err != nil {
			return result, err
		}
		if _, valid := auth.ParseLegacyPassword(a.PasswordHash); !valid {
			if !a.Disabled && !a.Deleted {
				result.UnusableCredentials++
			}
			a.Disabled, a.PasswordHash = true, ""
		}
		tag, err := tx.Exec(ctx, `INSERT INTO backoffice_system_accounts(area,scope,id,username,display_name,email,phone,gender,account_type,agency_level,
			administrator,disabled,deleted,remark,password_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT DO NOTHING`, area, row.Scope, a.ID.Value(), a.Username, a.DisplayName, a.Email, a.Phone, a.Gender, a.Type, a.AgencyLevel,
			a.Admin, a.Disabled, a.Deleted, a.Remark, a.PasswordHash, a.Created)
		if err := countLegacyBackofficeRow(tag, err, &result.Accounts, &result.Skipped); err != nil {
			return result, err
		}
		if tag.RowsAffected() == 1 {
			accounts[backofficeImportKey{row.Scope, a.ID.Value()}] = true
		}
	}
	for row, err := range realm.Roles {
		if err != nil {
			return result, err
		}
		r := row.Role
		if r.ID <= 0 || !validLegacySystemScope(area, row.Scope) || r.Name == "" {
			result.Skipped++
			continue
		}
		scopes[row.Scope] = true
		tag, err := tx.Exec(ctx, `INSERT INTO backoffice_system_roles(area,scope,id,name,code,sort,status,remark,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, area, row.Scope, r.ID.Value(), r.Name, r.Code, r.Sort, r.Status, r.Remark, r.Created)
		if err := countLegacyBackofficeRow(tag, err, &result.Roles, &result.Skipped); err != nil {
			return result, err
		}
		if tag.RowsAffected() == 1 {
			roles[backofficeImportKey{row.Scope, r.ID.Value()}] = true
		}
	}

	menuTrees := map[string]map[int64]admin.SystemMenu{}
	for row, err := range realm.Menus {
		if err != nil {
			return result, err
		}
		if row.Menu.ID <= 0 || row.Menu.Name == "" || area == admin.OperationArea && !validLegacySystemScope(area, row.Scope) {
			result.Skipped++
			continue
		}
		if menuTrees[row.Scope] == nil {
			menuTrees[row.Scope] = map[int64]admin.SystemMenu{}
		}
		menuTrees[row.Scope][row.Menu.ID.Value()] = row.Menu
	}
	if area == admin.MCNArea {
		rows, err := tx.Query(ctx, `SELECT DISTINCT scope FROM backoffice_system_bootstrap WHERE area='mcn'`)
		if err != nil {
			return result, err
		}
		reserved, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return result, err
		}
		for _, scope := range reserved {
			scopes[scope] = true
		}
		for scope := range scopes {
			menuTrees[scope] = menuTrees[""]
		}
		delete(menuTrees, "")
	}
	for scope, tree := range menuTrees {
		if err := importLegacySystemMenuTree(ctx, tx, area, scope, tree, menus, &result); err != nil {
			return result, err
		}
	}
	type grantSet struct {
		source          iter.Seq2[LegacySystemGrant, error]
		owners, targets map[backofficeImportKey]bool
		query           string
		count           *int
	}
	grants := []grantSet{
		{realm.Members, accounts, roles, `INSERT INTO backoffice_system_members(area,scope,account_id,role_id,group_admin)
			VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, &result.Members},
		{realm.RoleMenus, roles, menus, `INSERT INTO backoffice_system_role_menus(area,scope,role_id,menu_id)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, &result.RoleMenus},
		{realm.UserMenus, accounts, menus, `INSERT INTO backoffice_system_user_menus(area,scope,account_id,menu_id)
			VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, &result.UserMenus},
	}
	for i, grants := range grants {
		if grants.source == nil {
			continue
		}
		for grant, err := range grants.source {
			if err != nil {
				return result, err
			}
			if !grants.owners[backofficeImportKey{grant.Scope, grant.Owner}] ||
				!grants.targets[backofficeImportKey{grant.Scope, grant.Target}] {
				result.Skipped++
				continue
			}
			args := []any{area, grant.Scope, grant.Owner, grant.Target}
			if i == 0 {
				args = append(args, grant.GroupAdmin && area == admin.MCNArea)
			}
			tag, err := tx.Exec(ctx, grants.query, args...)
			if err := countLegacyBackofficeRow(tag, err, grants.count, &result.Skipped); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func importLegacySystemMenuTree(ctx context.Context, tx pgx.Tx, area, scope string,
	tree map[int64]admin.SystemMenu, inserted map[backofficeImportKey]bool, result *LegacyBackofficeResult) error {
	children := map[int64][]int64{}
	for id, menu := range tree {
		children[menu.Parent.Value()] = append(children[menu.Parent.Value()], id)
	}
	queue := children[0]
	slices.Sort(queue)
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		menu := tree[id]
		visited++
		if menu.Parent > 0 && !inserted[backofficeImportKey{scope, menu.Parent.Value()}] {
			result.Skipped++
		} else {
			tag, err := tx.Exec(ctx, `INSERT INTO backoffice_system_menus(area,scope,id,parent_id,name,url,menu_type,hidden,target,refresh,
				permission,icon,sort,remark,created_at) VALUES($1,$2,$3,NULLIF($4,0),$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
				ON CONFLICT DO NOTHING`, area, scope, id, menu.Parent.Value(), menu.Name, menu.URL, menu.Type, menu.Hidden, menu.Target,
				menu.Refresh, menu.Permission, menu.Icon, menu.Sort, menu.Remark, menu.Created)
			if err := countLegacyBackofficeRow(tag, err, &result.Menus, &result.Skipped); err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				inserted[backofficeImportKey{scope, id}] = true
			}
		}
		queue = append(queue, children[id]...)
	}
	// Unreachable rows have a dangling parent or a cycle. Neither can grant permissions.
	result.Skipped += len(tree) - visited
	return nil
}

func countLegacyBackofficeRow(tag pgconn.CommandTag, err error, count, skipped *int) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		*count++
	} else {
		*skipped++
	}
	return nil
}

func validLegacySystemScope(area, scope string) bool {
	if area == admin.OperationArea {
		return scope != "" && len(scope) <= 100 && strings.TrimSpace(scope) == scope
	}
	id, err := strconv.ParseInt(scope, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == scope
}
