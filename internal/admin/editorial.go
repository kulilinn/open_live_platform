package admin

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/catalog"
	"open_live_platform/internal/transport/httpx"
)

type CatalogOperation struct {
	Path     string
	Sections []string
	Write    bool
}
type catalogEditor interface {
	OperateCatalog(context.Context, Principal, CatalogOperation, func(catalog.Snapshot, int64) (any, error)) (any, error)
}

var errCatalogInput = errors.New("invalid catalog data")
var errCatalogInUse = errors.New("category has children or questions")

type editorialRequest struct {
	PageRequest
	broadcastFields
	ID                                                               string `json:"id"`
	PID                                                              *int64 `json:"pid,string"`
	Name, Title, Language, Content                                   string
	Sort                                                             uint16
	Status                                                           *int8
	IsHot                                                            int8
	First                                                            int64 `json:"firstClassifyId,string"`
	Second                                                           int64 `json:"secondClassifyId,string"`
	FilterClass                                                      int64 `json:"classifyId,string"`
	Platform                                                         int8
	CurrVersion, ContentZH, ContentJA, ContentEN, IosURL, AndroidURL string
}

func (b backoffice) registerEditorial(mux *http.ServeMux) {
	store, ok := b.store.(catalogEditor)
	if !ok {
		return
	}
	register := func(prefix string, sections []string, actions ...string) {
		for _, action := range actions {
			path := prefix + "/" + action
			op := CatalogOperation{Path: path, Sections: sections, Write: !slices.Contains([]string{"list", "page", "detail", "get_answer"}, action)}
			mux.HandleFunc("POST "+path, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
				if p.Tenant != OperationTenant {
					backofficeError(w, 403, "403", "forbidden")
					return
				}
				var input editorialRequest
				if !(sections[0] == "versions" && action == "page") && !httpx.Decode(w, r, &input) {
					return
				}
				result, err := store.OperateCatalog(r.Context(), p, op, func(data catalog.Snapshot, next int64) (any, error) {
					switch sections[0] {
					case "marquee", "broadcast_configs":
						return input.broadcast(action, sections[0], data, next)
					case "versions":
						return input.version(action, data)
					case "questions":
						return input.question(action, data, next, p)
					default:
						return input.category(action, data, next, p)
					}
				})
				if errors.Is(err, errCatalogInput) || errors.Is(err, errCatalogInUse) {
					backofficeError(w, 400, "400", err.Error())
					return
				}
				backofficeReply(w, result, err)
			}))
		}
	}
	register("/api/operation/help/category", []string{"classify", "questions"}, "list", "detail", "add", "edit", "delete")
	register("/api/operation/help/question", []string{"questions", "answers", "classify"}, "page", "detail", "add", "edit", "delete", "change_status", "get_answer")
	register("/api/operation/platform/version", []string{"versions"}, "page", "add_or_edit", "delete")
	register("/api/operation/broadcast/marquee", []string{"marquee"}, "page", "add", "edit", "delete")
	register("/api/operation/broadcast/config", []string{"broadcast_configs"}, "page", "add", "edit")
}

func value(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
func idText(n int64) string { return strconv.FormatInt(n, 10) }
func rowIndex(rows []catalog.Row, id int64, lang string) int {
	return slices.IndexFunc(rows, func(r catalog.Row) bool { return r.Int("id") == id && r.String("language") == lang })
}
func sortedRows(rows []catalog.Row, field string) {
	slices.SortStableFunc(rows, func(a, b catalog.Row) int {
		return cmp.Or(cmp.Compare(a.Int(field), b.Int(field)), cmp.Compare(a.Int("id"), b.Int("id")))
	})
}
func addMetadata(r catalog.Row, id int64, lang string, p Principal) {
	r["id"], r["language"], r["create_by"], r["create_by_name"], r["create_time"] = value(id), value(lang), value(p.ID), value(p.Name()), value(time.Now().UTC())
}

func (q editorialRequest) category(action string, d catalog.Snapshot, next int64, p Principal) (any, error) {
	rows := d["classify"]
	id, _ := strconv.ParseInt(q.ID, 10, 64)
	at := rowIndex(rows, id, q.Language)
	if action == "list" {
		out := []map[string]any{}
		for _, r := range rows {
			if (q.ID != "" && r.Int("id") != id) || (q.PID != nil && r.Int("pid") != *q.PID) || (q.Language != "" && q.Language != r.String("language")) {
				continue
			}
			out = append(out, map[string]any{"id": idText(r.Int("id")), "pid": idText(r.Int("pid")), "name": r.String("name"), "sort": r.Int("display_order")})
		}
		return out, nil
	}
	if q.Language == "" {
		return nil, errCatalogInput
	}
	if action == "delete" && at < 0 {
		return nil, nil
	}
	if action != "add" && at < 0 {
		return nil, ErrBackofficeNotFound
	}
	switch action {
	case "detail":
		return rows[at], nil
	case "delete":
		for _, r := range rows {
			if r.Int("pid") == id {
				return nil, errCatalogInUse
			}
		}
		for _, r := range d["questions"] {
			if r.Int("first_classify_id") == id || r.Int("second_classify_id") == id {
				return nil, errCatalogInUse
			}
		}
		d["classify"] = slices.Delete(rows, at, at+1)
	case "add", "edit":
		if q.PID == nil || q.Name == "" {
			return nil, errCatalogInput
		}
		if *q.PID != 0 {
			parent := rowIndex(rows, *q.PID, q.Language)
			if parent < 0 || rows[parent].Int("pid") != 0 || (action == "edit" && *q.PID == id) {
				return nil, errCatalogInput
			}
		}
		if action == "add" {
			at = len(rows)
			rows = append(rows, catalog.Row{})
			addMetadata(rows[at], next, q.Language, p)
		}
		row := rows[at]
		row["pid"], row["name"], row["display_order"], row["update_time"] = value(*q.PID), value(q.Name), value(q.Sort), value(time.Now().UTC())
		sortedRows(rows, "display_order")
		d["classify"] = rows
	}
	return nil, nil
}

func (q editorialRequest) question(action string, d catalog.Snapshot, next int64, p Principal) (any, error) {
	rows, answers, cats := d["questions"], d["answers"], d["classify"]
	id, _ := strconv.ParseInt(q.ID, 10, 64)
	at, answer := rowIndex(rows, id, q.Language), rowIndex(answers, id, q.Language)
	if action == "page" {
		out := []map[string]any{}
		catName := func(id int64, lang string) string {
			if i := rowIndex(cats, id, lang); i >= 0 {
				return cats[i].String("name")
			}
			return ""
		}
		list := slices.Clone(rows)
		sortedRows(list, "display_order")
		for _, r := range list {
			lang := r.String("language")
			first, second := r.Int("first_classify_id"), r.Int("second_classify_id")
			if (q.ID != "" && r.Int("id") != id) || (q.Language != "" && lang != q.Language) || (q.Status != nil && r.Int("status") != int64(*q.Status)) || !strings.HasPrefix(strings.ToLower(r.String("question")), strings.ToLower(q.Title)) || (q.FilterClass > 0 && first != q.FilterClass) {
				continue
			}
			out = append(out, map[string]any{
				"id": idText(r.Int("id")), "title": r.String("question"),
				"firstId": first, "firstName": catName(first, lang),
				"secondId": second, "secondName": catName(second, lang),
				"isHot": r.Int("is_hot"), "status": r.Int("status"),
			})
		}
		page, size, offset := q.Window()
		n := int64(len(out))
		return map[string]any{"list": out[min(n, offset):min(n, offset+size)], "current": page, "pageSize": size, "total": n}, nil
	}
	if q.Language == "" {
		return nil, errCatalogInput
	}
	if action != "add" && at < 0 {
		return nil, ErrBackofficeNotFound
	}
	switch action {
	case "get_answer":
		if answer < 0 {
			return nil, ErrBackofficeNotFound
		}
		return answers[answer].String("answer"), nil
	case "detail":
		r := rows[at]
		content := ""
		if answer >= 0 {
			content = answers[answer].String("answer")
		}
		return map[string]any{"id": q.ID, "title": r.String("question"), "firstClassifyId": idText(r.Int("first_classify_id")), "secondClassifyId": idText(r.Int("second_classify_id")), "language": q.Language, "content": content, "status": r.Int("status"), "isHot": r.Int("is_hot"), "sort": r.Int("display_order")}, nil
	case "delete":
		d["questions"] = slices.Delete(rows, at, at+1)
		if answer >= 0 {
			d["answers"] = slices.Delete(answers, answer, answer+1)
		}
	case "change_status":
		var status int8
		if q.Status != nil {
			status = *q.Status
		}
		rows[at]["status"], rows[at]["update_time"] = value(status), value(time.Now().UTC())
	case "add", "edit":
		first := rowIndex(cats, q.First, q.Language)
		if q.Title == "" || q.Status == nil || first < 0 || cats[first].Int("pid") != 0 {
			return nil, errCatalogInput
		}
		if q.Second != 0 {
			second := rowIndex(cats, q.Second, q.Language)
			if second < 0 || cats[second].Int("pid") != q.First {
				return nil, errCatalogInput
			}
		}
		if action == "add" {
			if slices.ContainsFunc(rows, func(r catalog.Row) bool {
				return strings.EqualFold(r.String("question"), q.Title) && (q.Second == 0 || r.Int("second_classify_id") == q.Second)
			}) {
				return nil, errCatalogInput
			}
			id = next
			at = len(rows)
			rows = append(rows, catalog.Row{})
			addMetadata(rows[at], id, q.Language, p)
		}
		if answer < 0 {
			answer = len(answers)
			answers = append(answers, catalog.Row{"id": value(id), "language": value(q.Language)})
		}
		answers[answer]["answer"] = value(q.Content)
		row := rows[at]
		row["first_classify_id"], row["second_classify_id"], row["question"], row["status"], row["display_order"], row["is_hot"] = value(q.First), value(q.Second), value(q.Title), value(*q.Status), value(q.Sort), value(q.IsHot)
		row["update_by_name"], row["update_time"] = value(p.Name()), value(time.Now().UTC())
		sortedRows(rows, "display_order")
		slices.SortStableFunc(rows, func(a, b catalog.Row) int { return cmp.Compare(b.Int("is_hot"), a.Int("is_hot")) })
		d["questions"], d["answers"] = rows, answers
	}
	return nil, nil
}

func (q editorialRequest) version(action string, d catalog.Snapshot) (any, error) {
	rows := d["versions"]
	name := q.Name
	if action == "delete" {
		name = q.ID
	}
	at := slices.IndexFunc(rows, func(r catalog.Row) bool { return r.String("name") == name })
	switch action {
	case "page":
		out := []map[string]any{}
		for _, r := range rows {
			if r.Int("del_flag") != 1 {
				continue
			}
			out = append(out, map[string]any{"name": r.String("name"), "platform": r.Int("platform"), "currVersion": r.String("curr_version"), "contentZh": r.String("content_zh"), "contentJa": r.String("content_ja"), "contentEn": r.String("content_en"), "iosUrl": r.String("ios_url"), "androidUrl": r.String("android_url"), "createTime": r["update_time"]})
		}
		return out, nil
	case "delete":
		if at >= 0 {
			rows[at]["del_flag"] = value(2)
			rows[at]["update_time"] = value(time.Now().UTC())
		}
	case "add_or_edit":
		if name == "" || q.CurrVersion == "" {
			return nil, errCatalogInput
		}
		if at < 0 {
			at = len(rows)
			rows = append(rows, catalog.Row{"name": value(name), "del_flag": value(1), "_version": value(nil)})
		}
		r := rows[at]
		r["platform"], r["curr_version"], r["content_zh"], r["content_ja"], r["content_en"], r["ios_url"], r["android_url"], r["update_time"] = value(q.Platform), value(q.CurrVersion), value(q.ContentZH), value(q.ContentJA), value(q.ContentEN), value(q.IosURL), value(q.AndroidURL), value(time.Now().UTC())
		d["versions"] = rows
	}
	return nil, nil
}
