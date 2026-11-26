package admin

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"open_live_platform/internal/transport/httpx"
)

func systemPermission(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", ":")
}

func systemReply(w http.ResponseWriter, data any, err error) {
	switch {
	case errors.Is(err, ErrSystemForbidden):
		backofficeError(w, http.StatusForbidden, "403", "Operation not allowed")
	case errors.Is(err, ErrSystemInvalid):
		backofficeError(w, http.StatusBadRequest, "400", "Invalid request")
	case errors.Is(err, ErrSystemConflict):
		backofficeError(w, http.StatusConflict, "409", "Record already exists or is in use")
	default:
		backofficeReply(w, data, err)
	}
}

func (b backoffice) registerSystem(mux *http.ServeMux) {
	store, ok := b.store.(SystemStore)
	if !ok {
		return
	}
	prefix, removal, password := "/api/system/", "delete", "change_pwd"
	if b.area == OperationArea {
		prefix, removal, password = "/api/operation/system/", "remove", "changepwd"
	}
	add := func(path string, permission bool, handle backofficeHandler) {
		mux.HandleFunc("POST "+prefix+path, b.authorizedTenant(false, permission, "", handle))
	}
	if b.area == OperationArea {
		add("user/info", false, func(w http.ResponseWriter, _ *http.Request, p Principal, _ [sha256.Size]byte) {
			backofficeReply(w, map[string]any{"id": strconv.FormatInt(p.ID, 10), "username": p.Username,
				"nickName": p.Nickname, "admin": p.Admin, "roles": nonNil(p.Roles), "permissions": nonNil(p.Permissions), "routes": nonNil(p.Routes)}, nil)
		})
		add("user/routes", true, func(w http.ResponseWriter, _ *http.Request, p Principal, _ [sha256.Size]byte) {
			backofficeReply(w, nonNil(p.Routes), nil)
		})
		add("user/operationlogpage", true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var query SystemFilter
			if httpx.Decode(w, r, &query) {
				page := query.page()
				list, total, err := b.store.BackofficeLogs(r.Context(), p, page)
				backofficePage(w, list, page, total, err)
			}
		})
		add("tenant/add", false, b.systemMutation(store, "tenant", "add", false))
	}
	add("user/detail", false, b.systemAccountList(store, "detail"))
	add("user/page", true, b.systemAccountList(store, "page"))
	add("user/"+password, b.area == MCNArea, b.systemMutation(store, "user", "password", b.area == MCNArea))
	add("role/list", false, b.systemRoleList(store, "list"))
	add("role/detail", false, b.systemRoleList(store, "detail"))
	add("role/page", true, b.systemRoleList(store, "page"))
	add("menu/list", false, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		menus, err := store.ListSystemMenus(r.Context(), p)
		out := make([]any, 0, len(menus))
		for _, menu := range menus {
			out = append(out, systemMenuResponse(p.Area, menu))
		}
		systemReply(w, out, err)
	})
	for _, resource := range []string{"user", "role", "menu"} {
		for _, action := range []string{"add", "edit"} {
			add(resource+"/"+action, true, b.systemMutation(store, resource, action, true))
		}
		if resource != "user" || b.area == MCNArea {
			add(resource+"/"+removal, true, b.systemMutation(store, resource, "delete", true))
		}
	}
	if b.area == MCNArea {
		add("user/manager_list", true, b.systemAccountList(store, "managers"))
		add("role/group_staff_list", true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var query SystemFilter
			if !httpx.Decode(w, r, &query) {
				return
			}
			list, err := store.ListSystemGroup(r.Context(), p, query.ID.Value())
			systemReply(w, nonNil(list), err)
		})
		for _, action := range []string{"set_group_admin", "clean_group_admin"} {
			add("role/"+action, true, b.systemMutation(store, "role", action, true))
		}
	}
}

func (filter SystemFilter) page() PageRequest {
	if filter.PageParams != nil {
		return *filter.PageParams
	}
	return filter.PageRequest
}

func (b backoffice) systemAccountList(store SystemStore, action string) backofficeHandler {
	return func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var filter SystemFilter
		if action != "managers" && !httpx.Decode(w, r, &filter) {
			return
		}
		if action == "detail" && filter.ID <= 0 {
			systemReply(w, nil, ErrSystemInvalid)
			return
		}
		filter.PageRequest = filter.page()
		filter.Managers, filter.All = action == "managers", action == "managers"
		if action == "detail" {
			filter.PageRequest = PageRequest{}
		}
		items, total, err := store.ListSystemAccounts(r.Context(), p, filter)
		if action == "detail" && err == nil {
			if len(items) == 0 {
				err = ErrBackofficeNotFound
			} else {
				systemReply(w, systemAccountResponse(p.Area, items[0], true), nil)
				return
			}
		}
		if err != nil {
			systemReply(w, nil, err)
			return
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, systemAccountResponse(p.Area, item, false))
		}
		if action == "managers" {
			systemReply(w, out, nil)
		} else if p.Area == OperationArea {
			systemRawPage(w, out, filter.PageRequest, total)
		} else {
			backofficePage(w, out, filter.PageRequest, total, nil)
		}
	}
}

func (b backoffice) systemRoleList(store SystemStore, action string) backofficeHandler {
	return func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var filter SystemFilter
		if action != "list" && !httpx.Decode(w, r, &filter) {
			return
		}
		if action == "detail" && filter.ID <= 0 {
			systemReply(w, nil, ErrSystemInvalid)
			return
		}
		filter.PageRequest, filter.All = filter.page(), action != "page"
		items, total, err := store.ListSystemRoles(r.Context(), p, filter)
		if err != nil {
			systemReply(w, nil, err)
			return
		}
		if action == "detail" {
			if len(items) == 0 {
				systemReply(w, nil, ErrBackofficeNotFound)
			} else {
				systemReply(w, systemRoleResponse(p.Area, items[0], action), nil)
			}
			return
		}
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, systemRoleResponse(p.Area, item, action))
		}
		if action == "page" {
			systemRawPage(w, out, filter.PageRequest, total)
		} else {
			systemReply(w, out, nil)
		}
	}
}

type systemMutationRequest struct {
	ID            SystemID   `json:"id"`
	UserID        SystemID   `json:"userId"`
	IDs           []SystemID `json:"ids"`
	MenuIDs       []SystemID `json:"menuIds"`
	RoleIDs       []SystemID `json:"roleIds"`
	UserPrivilege []SystemID `json:"userPrivilege"`
	Username      string     `json:"username"`
	Nickname      string     `json:"nickName"`
	Name          string     `json:"name"`
	Email         string     `json:"email"`
	Phone         string     `json:"phoneNumber"`
	Gender        int16      `json:"gender"`
	Type          int16      `json:"ty"`
	Admin         *bool      `json:"admin"`
	SuperAdmin    int16      `json:"superAdmin"`
	Remark        string     `json:"remark"`
	Password      string     `json:"password"`
	Passwd        string     `json:"passwd"`
	OldPassword   string     `json:"oldPassword"`
	NewPassword   string     `json:"newPassword"`
	Code          string     `json:"code"`
	Parent        SystemID   `json:"pid"`
	URL           string     `json:"url"`
	MenuType      int16      `json:"menuType"`
	Hidden        int16      `json:"hidden"`
	Permission    string     `json:"perms"`
	Icon          string     `json:"icon"`
	Sort          int32      `json:"sort"`
	DisplayOrder  *int32     `json:"displayOrder"`
	Status        *int16     `json:"status"`
}

func (b backoffice) systemMutation(store SystemStore, resource, action string, needsPermission bool) backofficeHandler {
	return func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
		var body systemMutationRequest
		if !httpx.Decode(w, r, &body) {
			return
		}
		change := SystemChange{Resource: resource, Action: action, Status: body.Status, IDs: body.IDs,
			Member: body.UserID, OldPassword: body.OldPassword}
		if needsPermission {
			change.Permission = systemPermission(r.URL.Path)
		}
		if body.DisplayOrder != nil {
			body.Sort = *body.DisplayOrder
		}
		isAdmin := body.Admin != nil && *body.Admin || body.SuperAdmin == 1
		change.Elevation = body.Admin != nil || body.SuperAdmin != 0
		if resource == "user" && b.area == MCNArea {
			if body.UserID > 0 {
				body.ID = body.UserID
			}
			body.Nickname, body.Password = body.Name, body.Passwd
		}
		if action == "password" {
			body.Password = body.NewPassword
		}
		change.Account = SystemAccount{ID: body.ID, Username: strings.TrimSpace(body.Username), Email: strings.TrimSpace(body.Email),
			DisplayName: body.Nickname, Phone: body.Phone, Remark: body.Remark, Gender: body.Gender, Type: body.Type,
			RoleIDs: body.RoleIDs, UserPrivilege: body.UserPrivilege, Admin: isAdmin}
		change.Role = SystemRole{ID: body.ID, Name: body.Name, Code: body.Code, Remark: body.Remark, Sort: body.Sort, MenuIDs: body.MenuIDs}
		change.Menu = SystemMenu{ID: body.ID, Parent: body.Parent, Name: body.Name, URL: body.URL, Type: body.MenuType,
			Hidden: body.Hidden, Target: 1, Permission: body.Permission, Icon: body.Icon, Sort: body.Sort, Remark: body.Remark}
		change.Tenant = SystemTenant{Code: body.Code, Name: body.Name, Remark: body.Remark}
		if action == "add" && resource == "user" && body.Password == "" || action == "password" || body.Password != "" && (resource == "user" || resource == "tenant") {
			hash, err := SystemPassword(body.Password)
			if err != nil {
				systemReply(w, nil, err)
				return
			}
			change.Account.PasswordHash = hash
		}
		data, err := store.ApplySystemChange(r.Context(), p, change)
		if p.Area == MCNArea && (resource == "role" || resource == "user" && action == "edit") {
			data = nil
		}
		if err == nil && data != nil {
			switch row := data.(type) {
			case SystemAccount:
				data = systemAccountResponse(p.Area, row, false)
			case SystemMenu:
				data = systemMenuResponse(p.Area, row)
			case SystemRole:
				data = systemRoleResponse(p.Area, row, action)
			}
		}
		systemReply(w, data, err)
	}
}

func systemRawPage(w http.ResponseWriter, list []any, query PageRequest, total int64) {
	page, size, _ := query.Window()
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]any{"list": nonNil(list), "current": page, "pageSize": size, "total": total})
}

func systemAccountResponse(area string, row SystemAccount, detail bool) any {
	status := 0
	if row.Disabled {
		status = 1
	}
	body := map[string]any{"id": row.ID, "email": row.Email, "status": status, "remark": row.Remark, "createTime": row.Created}
	if area == OperationArea {
		body["username"], body["nickName"], body["gender"], body["phoneNumber"] = row.Username, row.DisplayName, row.Gender, row.Phone
	} else {
		superAdmin := 0
		if row.Admin {
			superAdmin = 1
		}
		body["name"], body["ty"], body["isSuperadmin"] = row.DisplayName, row.Type, superAdmin
		body["roleName"], body["managerName"], body["roleManagerName"] = row.RoleName, row.ManagerName, row.ManagerName
		body["agencyLevel"], body["isDel"] = row.AgencyLevel, 0
	}
	if detail {
		body["roleIds"] = nonNil(row.RoleIDs)
		if area == MCNArea {
			body["userPrivilege"] = nonNil(row.UserPrivilege)
		}
	}
	return body
}

func systemRoleResponse(area string, row SystemRole, action string) any {
	if area == MCNArea && action == "page" {
		return map[string]any{"id": row.ID, "name": row.Name, "managerName": row.ManagerName, "staffNum": row.Staff}
	}
	body := map[string]any{"id": row.ID, "name": row.Name, "status": row.Status, "remark": row.Remark, "createTime": row.Created}
	if area == OperationArea {
		body["code"] = row.Code
	}
	if area == MCNArea || action == "detail" {
		body["sort"] = row.Sort
	} else {
		body["displayOrder"] = row.Sort
	}
	if action == "detail" {
		body["menuIds"] = nonNil(row.MenuIDs)
	}
	return body
}

func systemMenuResponse(area string, row SystemMenu) any {
	if area != OperationArea {
		return row
	}
	return map[string]any{"id": row.ID, "pid": row.Parent, "name": row.Name, "url": row.URL, "target": row.Target,
		"menuType": row.Type, "hidden": row.Hidden, "refresh": row.Refresh, "perms": row.Permission, "icon": row.Icon,
		"displayOrder": row.Sort, "remark": row.Remark, "createTime": row.Created}
}
