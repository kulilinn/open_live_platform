package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"iter"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/go-sql-driver/mysql"
	"open_live_platform/internal/admin"
	"open_live_platform/internal/store/postgres"
)

func main() {
	source := flag.String("mysql-dsn", os.Getenv("LIVEMEDIA_MYSQL_DSN"), "legacy MySQL read-only DSN")
	target := flag.String("database-url", os.Getenv("DATABASE_URL"), "target PostgreSQL URL")
	bootstrap := flag.String("bootstrap-file", os.Getenv("ADMIN_ACCOUNTS_FILE"), "deployment bootstrap credentials JSON")
	area := flag.String("area", "all", "all, admin, or mcn")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *source, *target, *bootstrap, *area); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, source, target, bootstrap, area string) error {
	if source == "" || target == "" || bootstrap == "" || (area != "all" && area != admin.OperationArea && area != admin.MCNArea) {
		return errors.New("provide MySQL DSN, PostgreSQL URL, the deployment bootstrap credentials file, and a valid area")
	}
	principals, err := admin.ReadPrincipals(bootstrap)
	if err != nil {
		return err
	}
	config, err := mysql.ParseDSN(source)
	if err != nil {
		return errors.New("invalid legacy MySQL DSN")
	}
	config.ParseTime, config.Loc = true, time.UTC
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return errors.New("invalid legacy MySQL DSN")
	}
	old := sql.OpenDB(connector)
	defer old.Close()
	snapshot, err := old.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("read legacy MySQL snapshot: %w", err)
	}
	defer snapshot.Rollback()
	db, err := postgres.Open(ctx, target)
	if err != nil {
		return fmt.Errorf("open target PostgreSQL: %w", err)
	}
	defer db.Shutdown()
	if err := db.ReserveSystemBootstrap(ctx, principals); err != nil {
		return err
	}

	var areas []postgres.LegacyBackoffice
	if area == "all" || area == admin.OperationArea {
		roleOrder, err := legacyAdminRoleOrder(ctx, snapshot)
		if err != nil {
			return err
		}
		areas = append(areas, postgres.LegacyBackoffice{
			Area:      admin.OperationArea,
			Tenants:   sourceRows(ctx, snapshot, "admin_tenant", operationTenants, scanTenant),
			Accounts:  sourceRows(ctx, snapshot, "admin_user", operationUsers, scanAccount),
			Roles:     sourceRows(ctx, snapshot, "admin_role", fmt.Sprintf(operationRoles, roleOrder), scanRole),
			Menus:     sourceRows(ctx, snapshot, "admin_menu", operationMenus, scanMenu),
			Members:   sourceRows(ctx, snapshot, "admin_user_role", operationMembers, scanGrant),
			RoleMenus: sourceRows(ctx, snapshot, "admin_role_menu", operationRoleMenus, scanGrant),
		})
	}
	if area == "all" || area == admin.MCNArea {
		areas = append(areas, postgres.LegacyBackoffice{
			Area:      admin.MCNArea,
			Accounts:  sourceRows(ctx, snapshot, "mcn_user", mcnUsers, scanAccount),
			Roles:     sourceRows(ctx, snapshot, "mcn_role", mcnRoles, scanRole),
			Menus:     sourceRows(ctx, snapshot, "mcn_menu", mcnMenus, scanMenu),
			Members:   sourceRows(ctx, snapshot, "mcn_user_role", mcnMembers, scanGrant),
			RoleMenus: sourceRows(ctx, snapshot, "mcn_role_menu", mcnRoleMenus, scanGrant),
			UserMenus: sourceRows(ctx, snapshot, "mcn_user_privilege", mcnUserMenus, scanGrant),
		})
	}
	results, err := db.ImportLegacyBackoffice(ctx, areas)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(results)
}

const operationTenants = `SELECT id,code,name,remark,create_time FROM admin_tenant ORDER BY id`
const operationUsers = `SELECT id,tenant_code,username,nick_name,email,phone_number,gender,0,0,admin,status,del_flag,remark,password,create_time
	FROM admin_user ORDER BY id`
const operationRoles = `SELECT id,tenant_code,name,code,%s,status,remark,create_time FROM admin_role ORDER BY id`
const operationMenus = `SELECT id,tenant_code,pid,name,url,menu_type,hidden,target,refresh,perms,icon,display_order,remark,create_time
	FROM admin_menu ORDER BY id`
const operationMembers = `SELECT tenant_code,uid,rid,0 FROM admin_user_role ORDER BY id`
const operationRoleMenus = `SELECT tenant_code,rid,mid,0 FROM admin_role_menu ORDER BY id`

const mcnUsers = `SELECT id,CAST(mcn_id AS CHAR),'',name,email,'',0,ty,agency_level,is_superadmin,status,is_del,COALESCE(remark,''),password,create_time
	FROM mcn_user ORDER BY id`
const mcnRoles = `SELECT id,CAST(mcn_id AS CHAR),name,'',sort,status,COALESCE(remark,''),create_time FROM mcn_role ORDER BY id`
const mcnMenus = `SELECT id,'',pid,name,url,menu_type,hidden,target,0,perms,COALESCE(icon,''),sort,COALESCE(remark,''),create_time FROM mcn_menu ORDER BY id`
const mcnMembers = `SELECT CAST(COALESCE(u.mcn_id,0) AS CHAR),m.user_id,m.role_id,m.group_admin
	FROM mcn_user_role m LEFT JOIN mcn_user u ON u.id=m.user_id ORDER BY m.id`
const mcnRoleMenus = `SELECT CAST(COALESCE(r.mcn_id,0) AS CHAR),m.role_id,m.role_menu_id,0
	FROM mcn_role_menu m LEFT JOIN mcn_role r ON r.id=m.role_id ORDER BY m.id`
const mcnUserMenus = `SELECT CAST(mcn_id AS CHAR),user_id,menu_id,0 FROM mcn_user_privilege ORDER BY id`

func legacyAdminRoleOrder(ctx context.Context, snapshot *sql.Tx) (string, error) {
	// The checked-in bootstrap SQL uses display_order. The newer ORM uses sort.
	var modern bool
	err := snapshot.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name='admin_role' AND column_name='sort')`).Scan(&modern)
	if err != nil {
		return "", fmt.Errorf("identify admin_role ordering: %w", err)
	}
	if modern {
		return "sort", nil
	}
	return "display_order", nil
}

func sourceRows[T any](ctx context.Context, snapshot *sql.Tx, table, query string, scan func(*sql.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := snapshot.QueryContext(ctx, query)
		if err != nil {
			yield(zero, fmt.Errorf("read %s: %w", table, err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scan(rows)
			if err != nil {
				yield(zero, fmt.Errorf("invalid data in %s: %w", table, err))
				return
			}
			if !yield(item, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, fmt.Errorf("read %s: %w", table, err))
		}
	}
}

func scanTenant(rows *sql.Rows) (entry admin.SystemTenant, err error) {
	var id int64
	err = rows.Scan(&id, &entry.Code, &entry.Name, &entry.Remark, &entry.Created)
	entry.ID = admin.SystemID(id)
	return
}

func scanAccount(rows *sql.Rows) (entry postgres.LegacySystemAccount, err error) {
	a := &entry.Account
	var id int64
	var adm, status, deleted int
	err = rows.Scan(&id, &entry.Scope, &a.Username, &a.DisplayName, &a.Email, &a.Phone, &a.Gender, &a.Type, &a.AgencyLevel,
		&adm, &status, &deleted, &a.Remark, &a.PasswordHash, &a.Created)
	a.ID, a.Admin, a.Disabled, a.Deleted = admin.SystemID(id), adm == 1, status != 0, deleted != 0
	return
}

func scanRole(rows *sql.Rows) (entry postgres.LegacySystemRole, err error) {
	role := &entry.Role
	var id int64
	err = rows.Scan(&id, &entry.Scope, &role.Name, &role.Code, &role.Sort, &role.Status, &role.Remark, &role.Created)
	role.ID = admin.SystemID(id)
	return
}

func scanMenu(rows *sql.Rows) (entry postgres.LegacySystemMenu, err error) {
	menu := &entry.Menu
	var id, parent int64
	err = rows.Scan(&id, &entry.Scope, &parent, &menu.Name, &menu.URL, &menu.Type, &menu.Hidden, &menu.Target, &menu.Refresh,
		&menu.Permission, &menu.Icon, &menu.Sort, &menu.Remark, &menu.Created)
	menu.ID, menu.Parent = admin.SystemID(id), admin.SystemID(parent)
	return
}

func scanGrant(rows *sql.Rows) (entry postgres.LegacySystemGrant, err error) {
	var group int
	err = rows.Scan(&entry.Scope, &entry.Owner, &entry.Target, &group)
	entry.GroupAdmin = group == 1
	return
}
