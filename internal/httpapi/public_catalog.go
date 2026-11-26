package httpapi

import (
	"cmp"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"open_live_platform/internal/catalog"
	"open_live_platform/internal/transport/httpx"
)

func RegisterPublicCatalog(mux *http.ServeMux, store catalog.Store) {
	read := func(w http.ResponseWriter, r *http.Request, key string) ([]catalog.Row, bool) {
		rows, err := store.CatalogRows(r.Context(), key)
		if err != nil {
			httpx.Error(w, 500, "could not read catalog")
			return nil, false
		}
		return rows, true
	}
	reply := func(w http.ResponseWriter, v any) { httpx.JSON(w, 200, map[string]any{"status": "200", "data": v}) }
	missing := func(w http.ResponseWriter) {
		httpx.JSON(w, 200, map[string]string{"status": "4005", "message": "data not found "})
	}
	mux.HandleFunc("POST /api/app/list", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Offset int64
			Order  string `json:"order_type"`
		}
		if !httpx.Decode(w, r, &q) {
			return
		}
		order := cmp.Or(q.Order, "ReleaseTime")
		if q.Offset < 0 || (order != "ReleaseTime" && order != "Popularity" && order != "Installs") {
			httpx.Error(w, 400, "invalid offset or order_type")
			return
		}
		rows, ok := read(w, r, "apps")
		if !ok {
			return
		}
		apps := make([]catalog.Row, 0, len(rows))
		for _, row := range rows {
			if row.Int("state") > 1 {
				public := maps.Clone(row)
				delete(public, "app_secret")
				apps = append(apps, public)
			}
		}
		slices.SortStableFunc(apps, func(a, b catalog.Row) int {
			switch order {
			case "Popularity":
				return cmp.Compare(b.Int("popularity"), a.Int("popularity"))
			case "Installs":
				return cmp.Compare(b.Int("installs"), a.Int("installs"))
			default:
				first, _ := time.Parse(time.RFC3339Nano, a.String("release_time"))
				second, _ := time.Parse(time.RFC3339Nano, b.String("release_time"))
				return second.Compare(first)
			}
		})
		start := min(int64(len(apps)), q.Offset)
		reply(w, apps[start:min(start+20, int64(len(apps)))])
	})
	mux.HandleFunc("GET /countries/{code}", func(w http.ResponseWriter, r *http.Request) {
		rows, ok := read(w, r, "countries")
		if !ok {
			return
		}
		var japan catalog.Row
		for _, v := range rows {
			switch v.String("code") {
			case r.PathValue("code"):
				httpx.JSON(w, 200, v)
				return
			case "JP":
				japan = v
			}
		}
		if japan == nil {
			missing(w)
			return
		}
		httpx.JSON(w, 200, japan)
	})
	mux.HandleFunc("POST /check_version/{ty}", func(w http.ResponseWriter, r *http.Request) {
		rows, ok := read(w, r, "versions")
		if !ok {
			return
		}
		for _, v := range rows {
			if v.String("name") == r.PathValue("ty") {
				reply(w, v)
				return
			}
		}
		missing(w)
	})
	mux.HandleFunc("GET /api/marquee/list", func(w http.ResponseWriter, r *http.Request) {
		rows, ok := read(w, r, "marquee")
		if !ok {
			return
		}
		list := make([]map[string]any, 0, len(rows))
		for _, v := range rows {
			list = append(list, map[string]any{"id": string(v["id"]), "name": v["name"], "isJump": v["is_jump"], "jumpUrl": v["jump_url"], "content": v["content"]})
		}
		reply(w, list)
	})
	question := func(v catalog.Row) map[string]any {
		return map[string]any{"id": string(v["id"]), "isHot": v.Int("is_hot") != 0, "question": v["question"]}
	}
	help := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lang := q.Get("language")
		if lang == "" {
			httpx.Error(w, 400, "language required")
			return
		}
		idParam := func(key string) (int64, bool) {
			v, err := strconv.ParseInt(q.Get(key), 10, 64)
			if err != nil {
				httpx.Error(w, 400, key+" required")
			}
			return v, err == nil
		}
		classes := []catalog.Row{}
		action := strings.TrimPrefix(r.URL.Path, "/api/help/")
		if action != "hotquestionlist" {
			rows, ok := read(w, r, "classify")
			if !ok {
				return
			}
			for _, v := range rows {
				if v.String("language") == lang {
					classes = append(classes, v)
				}
			}
		}
		findClass := func(id int64) catalog.Row {
			for _, v := range classes {
				if v.Int("id") == id {
					return v
				}
			}
			return nil
		}
		switch action {
		case "classifylist":
			result := []map[string]any{}
			for _, v := range classes {
				if v.Int("pid") == 0 {
					result = append(result, map[string]any{"id": string(v["id"]), "name": v["name"]})
				}
			}
			reply(w, result)
		case "classifydetail", "hotquestionlist":
			rows, ok := read(w, r, "questions")
			if !ok {
				return
			}
			if action == "hotquestionlist" {
				items := []map[string]any{}
				for _, v := range rows {
					if v.String("language") == lang && v.Int("is_hot") == 1 {
						items = append(items, question(v))
					}
				}
				reply(w, items)
				return
			}
			id, ok := idParam("firstClassifyId")
			if !ok {
				return
			}
			main := findClass(id)
			if main == nil {
				missing(w)
				return
			}
			byClass := make(map[int64][]map[string]any)
			for _, v := range rows {
				if v.String("language") == lang && v.Int("first_classify_id") == id {
					k := v.Int("second_classify_id")
					byClass[k] = append(byClass[k], question(v))
				}
			}
			children := []map[string]any{}
			for _, v := range classes {
				if v.Int("pid") == id {
					items := byClass[v.Int("id")]
					if items == nil {
						items = []map[string]any{}
					}
					children = append(children, map[string]any{"secondClassifyId": string(v["id"]), "secondClassifyName": v["name"], "questionList": items})
				}
			}
			reply(w, map[string]any{"firstClassifyId": string(main["id"]), "firstClassifyName": main["name"], "secondClassifyList": children})
		case "getanswer":
			id, ok := idParam("questionId")
			if !ok {
				return
			}
			data := map[string]any{"id": strconv.FormatInt(id, 10), "firstClassifyName": nil, "secondClassifyName": nil}
			for _, p := range []string{"firstClassify", "secondClassify"} {
				k := p + "Id"
				if !q.Has(k) {
					continue
				}
				n, ok := idParam(k)
				if !ok {
					return
				}
				v := findClass(n)
				if v == nil {
					missing(w)
					return
				}
				data[p+"Name"] = v["name"]
			}
			rows, ok := read(w, r, "answers")
			if !ok {
				return
			}
			for _, v := range rows {
				if v.String("language") == lang && v.Int("id") == id {
					data["answer"] = v["answer"]
					reply(w, data)
					return
				}
			}
			missing(w)
		default:
			http.NotFound(w, r)
		}
	}
	for _, action := range []string{"classifylist", "classifydetail", "hotquestionlist", "getanswer"} {
		mux.HandleFunc("GET /api/help/"+action, help)
	}
}
