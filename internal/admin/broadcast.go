package admin

import (
	"slices"
	"strconv"
	"strings"

	"open_live_platform/internal/catalog"
)

type broadcastFields struct {
	Format                                        int8
	JumpType, DisplayOrder                        int32
	JumpTarget, JumpName, JumpBackground, JumpURL *string
	Background, ContentKey                        string
	IsJump                                        int8
}

func (q editorialRequest) broadcast(action, section string, data catalog.Snapshot, next int64) (any, error) {
	rows := data[section]
	marquee := section == "marquee"
	id, parseErr := strconv.ParseInt(q.ID, 10, 64)
	at := slices.IndexFunc(rows, func(r catalog.Row) bool { return r.Int("id") == id })
	if action == "page" {
		list := []map[string]any{}
		for _, r := range rows {
			if marquee {
				if r.Int("is_deleted") != 0 {
					continue
				}
				status := r.Int("status")
				if r["status"] == nil {
					status = 1
				}
				list = append(list, map[string]any{"id": idText(r.Int("id")), "name": r["name"], "isJump": r["is_jump"], "status": status, "displayOrder": r.Int("display_order"), "jumpUrl": r["jump_url"], "contentKey": r["content"]})
			} else {
				if (q.ID != "" && r.Int("id") != id) || (q.Name != "" && r.String("name") != q.Name) {
					continue
				}
				list = append(list, map[string]any{"id": idText(r.Int("id")), "name": r["name"], "format": r.Int("format"), "jumpType": r.Int("jump_type"), "jumpTarget": r["jump_target"], "jumpBackground": r["jump_background"], "jumpName": r["jump_name"], "background": r["background"]})
			}
		}
		page, size, offset := q.Window()
		total := int64(len(list))
		return map[string]any{"list": list[min(total, offset):min(total, offset+size)], "current": page, "pageSize": size, "total": total}, nil
	}
	if !(marquee && action == "add") && (parseErr != nil || id <= 0) {
		return nil, errCatalogInput
	}
	if action == "delete" {
		if at < 0 {
			return nil, ErrBackofficeNotFound
		}
		rows[at]["is_deleted"] = value(1)
		return nil, nil
	}
	if q.Name == "" || (marquee && q.Status == nil) {
		return nil, errCatalogInput
	}
	if action == "add" {
		if marquee {
			if slices.ContainsFunc(rows, func(r catalog.Row) bool {
				return r.Int("is_deleted") == 0 && strings.EqualFold(r.String("name"), q.Name)
			}) {
				return nil, errCatalogInput
			}
			id, at = next, -1
		} else if at >= 0 {
			return nil, errCatalogInput
		}
	}
	if at < 0 {
		if marquee && action == "edit" {
			return nil, ErrBackofficeNotFound
		}
		at = len(rows)
		rows = append(rows, catalog.Row{"id": value(id)})
	}
	row := rows[at]
	row["name"] = value(q.Name)
	if marquee {
		row["is_jump"], row["status"], row["display_order"] = value(q.IsJump), value(*q.Status), value(q.DisplayOrder)
		row["content"], row["jump_url"] = value(q.ContentKey), value(q.JumpURL)
		sortedRows(rows, "display_order")
	} else {
		row["format"], row["jump_type"], row["background"] = value(q.Format), value(q.JumpType), value(q.Background)
		row["jump_target"], row["jump_name"], row["jump_background"] = value(q.JumpTarget), value(q.JumpName), value(q.JumpBackground)
		sortedRows(rows, "id")
	}
	data[section] = rows
	return nil, nil
}
