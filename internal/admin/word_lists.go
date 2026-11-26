package admin

import (
	"crypto/sha256"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"open_live_platform/internal/catalog"
	"open_live_platform/internal/transport/httpx"
	"open_live_platform/internal/upload"
)

type wordListInput struct {
	PageRequest
	ID                                int64 `json:"id,string"`
	name, description, country, scene string
	status                            int64
	file                              []byte
}

func readWordList(w http.ResponseWriter, r *http.Request) (wordListInput, bool) {
	var q wordListInput
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		backofficeError(w, 400, "400", "Invalid multipart form")
		return q, false
	}
	defer r.MultipartForm.RemoveAll()
	q.description, q.country, q.scene = r.FormValue("description"), r.FormValue("forCountry"), r.FormValue("forScience")
	q.ID, _ = strconv.ParseInt(r.FormValue("id"), 10, 64)
	if status := r.FormValue("status"); status != "" {
		n, err := strconv.ParseInt(status, 10, 8)
		if err != nil {
			backofficeError(w, 400, "400", "Invalid status")
			return q, false
		}
		q.status = n
	}
	for _, files := range r.MultipartForm.File {
		if len(files) != 1 || q.name != "" || !upload.ValidName(files[0].Filename) {
			backofficeError(w, 400, "400", "Expected one word list")
			return q, false
		}
		f, err := files[0].Open()
		if err != nil {
			backofficeError(w, 400, "400", "Invalid word list")
			return q, false
		}
		q.name = files[0].Filename
		q.file, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			backofficeError(w, 400, "400", "Invalid word list")
			return q, false
		}
	}
	return q, true
}

func (b backoffice) registerWordLists(mux *http.ServeMux) {
	store, ok := b.store.(catalogEditor)
	if !ok {
		return
	}
	for _, action := range []string{"page", "add", "edit", "delete"} {
		path := "/api/operation/system/bad_words/" + action
		op := CatalogOperation{Path: path, Write: action != "page", Sections: []string{"bad_words"}}
		mux.HandleFunc("POST "+path, b.authorized(false, true, func(w http.ResponseWriter, r *http.Request, p Principal, _ [sha256.Size]byte) {
			var q wordListInput
			if action == "add" || action == "edit" {
				var ok bool
				q, ok = readWordList(w, r)
				if !ok {
					return
				}
			} else if !httpx.Decode(w, r, &q) {
				return
			}
			data, err := store.OperateCatalog(r.Context(), p, op, func(d catalog.Snapshot, next int64) (any, error) {
				rows := d["bad_words"]
				if action == "page" {
					page, size, offset := q.Window()
					total := int64(len(rows))
					list := []map[string]any{}
					for _, r := range rows[min(total, offset):min(total, offset+size)] {
						list = append(list, map[string]any{"id": idText(r.Int("id")), "name": r["name"], "description": r["description"], "forCountry": r["for_country"], "forScience": r["for_science"], "status": r.Int("status")})
					}
					return map[string]any{"list": list, "current": page, "pageSize": size, "total": total}, nil
				}
				i := slices.IndexFunc(rows, func(r catalog.Row) bool { return r.Int("id") == q.ID })
				if action == "delete" {
					if i >= 0 {
						d["bad_words"] = slices.Delete(rows, i, i+1)
					}
					return nil, nil
				}
				if action == "edit" && i < 0 {
					return nil, ErrBackofficeNotFound
				}
				if action == "add" {
					if q.name == "" || slices.ContainsFunc(rows, func(r catalog.Row) bool { return strings.EqualFold(r.String("name"), q.name) }) {
						return nil, errCatalogInput
					}
					i = len(rows)
					rows = append(rows, catalog.Row{"id": value(next)})
				}
				row := rows[i]
				if q.name != "" {
					row["name"], row["file"] = value(q.name), value(q.file)
				}
				row["description"], row["for_country"], row["for_science"], row["status"] = value(q.description), value(q.country), value(q.scene), value(q.status)
				d["bad_words"] = rows
				return nil, nil
			})
			if err == errCatalogInput {
				backofficeError(w, 400, "400", err.Error())
				return
			}
			backofficeReply(w, data, err)
		}))
	}
}
