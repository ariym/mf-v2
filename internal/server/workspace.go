package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
)

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return err
	}
	return hub.DecodeStrict(raw, v)
}
func workspaceError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, sql.ErrNoRows) {
		status = 404
	}
	if errors.Is(err, jobs.ErrStopping) {
		status = 503
	}
	apiError(w, status, err)
}
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid ID")
	}
	return id, nil
}
func pagination(r *http.Request) (int, int, error) {
	limit, offset := 100, 0
	var err error
	if v := r.URL.Query().Get("limit"); v != "" {
		limit, err = strconv.Atoi(v)
		if err != nil {
			return 0, 0, err
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil {
			return 0, 0, err
		}
	}
	if limit < 1 || limit > 1000 || offset < 0 {
		return 0, 0, fmt.Errorf("invalid pagination")
	}
	return limit, offset, nil
}
func slicePage[T any](items []T, limit, offset int) []T {
	if offset >= len(items) {
		return []T{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}

func workspaceAPI(mux *http.ServeMux, c config.Config, s *store.Store, j *jobs.Engine) {
	mux.HandleFunc("GET /api/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, map[string]any{"workspace_version": 1, "schema_version": 4, "flow_protocols": []int{1, 2}, "builtin_producers": map[string]int{"extract_audio": 1}, "os": runtime.GOOS, "arch": runtime.GOARCH})
	})
	mux.HandleFunc("POST /api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		var in selectionRequest
		if err := decodeBody(w, r, &in); err != nil {
			workspaceError(w, err)
			return
		}
		items, err := selectClips(r.Context(), s, c.SQLitePath, in)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /api/v1/schema", func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.DB.QueryContext(r.Context(), `SELECT name,sql FROM sqlite_master WHERE type='table' AND name IN ('media_files','file_audio_metadata','file_video_metadata','segments','segment_tags','tag','tagtype','flow','program') ORDER BY name`)
		if err != nil {
			apiError(w, 500, err)
			return
		}
		defer rows.Close()
		out := []map[string]string{}
		for rows.Next() {
			var name, definition string
			if err = rows.Scan(&name, &definition); err != nil {
				apiError(w, 500, err)
				return
			}
			out = append(out, map[string]string{"name": name, "sql": definition})
		}
		if err = rows.Err(); err != nil {
			apiError(w, 500, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": out})
	})
	mux.HandleFunc("GET /api/v1/media/{id}/annotations", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		if _, err = s.Get(r.Context(), id); err != nil {
			workspaceError(w, err)
			return
		}
		segments, err := s.Segments(r.Context(), id)
		if err != nil {
			apiError(w, 500, err)
			return
		}
		tags, err := s.Tags(r.Context(), id)
		if err != nil {
			apiError(w, 500, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"segments": segments, "tags": tags})
	})
	annotationAPI(mux, s)
	draftAPI(mux, c, s, j)
	moduleAPI(mux, c, s)
	mux.HandleFunc("GET /api/v1/jobs/{id}/submission", func(w http.ResponseWriter, r *http.Request) {
		var flow string
		var version int
		var raw, paths string
		err := s.DB.QueryRowContext(r.Context(), "SELECT flow_key,flow_version,input,paths FROM job_metadata WHERE id=?", r.PathValue("id")).Scan(&flow, &version, &raw, &paths)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				apiError(w, 404, fmt.Errorf("this job has no saved submission snapshot; older jobs cannot be copied"))
				return
			}
			workspaceError(w, err)
			return
		}
		if version > 0 {
			flow += fmt.Sprintf("@%d", version)
		}
		data := map[string]any{"kind": "raw"}
		var targets []store.Target
		var targetJSON string
		targetErr := s.DB.QueryRowContext(r.Context(), "SELECT targets FROM job_execution WHERE job_id=?", r.PathValue("id")).Scan(&targetJSON)
		if targetErr == nil {
			if err = json.Unmarshal([]byte(targetJSON), &targets); err != nil {
				apiError(w, 500, err)
				return
			}
			// The returned document uses a catalog selector; SubmitPaths supplies media.
			var inputFields map[string]json.RawMessage
			if err = json.Unmarshal([]byte(raw), &inputFields); err != nil {
				apiError(w, 500, err)
				return
			}
			delete(inputFields, "media")
			normalized, err := json.Marshal(inputFields)
			if err != nil {
				apiError(w, 500, err)
				return
			}
			raw = string(normalized)
			ids := []int64{}
			mediaIDs := []int64{}
			allSegments := true
			for _, target := range targets {
				ids = append(ids, target.SegmentID)
				mediaIDs = append(mediaIDs, target.MediaFileID)
				if target.SegmentID == 0 {
					allSegments = false
				}
			}
			if allSegments {
				data = map[string]any{"kind": "segments", "segment_ids": ids}
			} else {
				data = map[string]any{"kind": "media", "media_ids": mediaIDs}
			}
		} else if !errors.Is(targetErr, sql.ErrNoRows) {
			apiError(w, 500, targetErr)
			return
		}
		jsonResponse(w, 200, map[string]any{"flow": flow, "data": data, "input": json.RawMessage(raw), "original_paths": json.RawMessage(paths), "targets": targets})
	})
}

func annotationAPI(mux *http.ServeMux, s *store.Store) {
	mux.HandleFunc("GET /api/v1/segments", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("media_file_id"), 10, 64)
		if err != nil || id < 1 {
			workspaceError(w, fmt.Errorf("media_file_id is required"))
			return
		}
		items, err := s.Segments(r.Context(), id)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /api/v1/segments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		v, err := s.Segment(r.Context(), id)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, v)
	})
	for _, method := range []string{"POST", "PATCH"} {
		pattern := method + " /api/v1/segments"
		if method == "PATCH" {
			pattern += "/{id}"
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				MediaID int64  `json:"media_file_id"`
				Start   string `json:"start"`
				End     string `json:"end"`
			}
			if err := decodeBody(w, r, &in); err != nil {
				workspaceError(w, err)
				return
			}
			var id int64
			var err error
			if r.Method == "POST" {
				id, err = s.AddSegment(r.Context(), in.MediaID, in.Start, in.End)
			} else {
				id, err = pathID(r)
				if err == nil {
					err = s.UpdateSegment(r.Context(), id, in.Start, in.End)
				}
			}
			if err != nil {
				workspaceError(w, err)
				return
			}
			v, err := s.Segment(r.Context(), id)
			if err != nil {
				workspaceError(w, err)
				return
			}
			jsonResponse(w, 200, v)
		})
	}
	mux.HandleFunc("DELETE /api/v1/segments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err == nil {
			err = s.RemoveSegment(r.Context(), id)
		}
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"removed": true})
	})
	mux.HandleFunc("GET /api/v1/tags", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.URL.Query().Get("media_file_id"), 10, 64)
		if err != nil || id < 1 {
			workspaceError(w, fmt.Errorf("media_file_id is required"))
			return
		}
		items, err := s.Tags(r.Context(), id)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("POST /api/v1/tags", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key     string          `json:"key"`
			Version int             `json:"version"`
			Value   json.RawMessage `json:"value"`
			Links   []store.TagLink `json:"links"`
		}
		if err := decodeBody(w, r, &in); err != nil {
			workspaceError(w, err)
			return
		}
		if in.Version < 1 {
			workspaceError(w, fmt.Errorf("tag type version must be positive"))
			return
		}
		id, err := s.AddTag(r.Context(), in.Key, in.Version, in.Value, in.Links)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 201, map[string]int64{"id": id})
	})
	mux.HandleFunc("PATCH /api/v1/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		var in struct {
			Value json.RawMessage `json:"value"`
		}
		if err = decodeBody(w, r, &in); err == nil {
			err = s.UpdateTag(r.Context(), id, in.Value)
		}
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]int64{"id": id})
	})
	mux.HandleFunc("DELETE /api/v1/tags/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err == nil {
			err = s.RemoveTag(r.Context(), id)
		}
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"removed": true})
	})
}

type jobDefinition struct {
	Flow string `json:"flow"`
	Data struct {
		Kind       string   `json:"kind"`
		SegmentIDs []int64  `json:"segment_ids,omitempty"`
		MediaIDs   []int64  `json:"media_ids,omitempty"`
		Paths      []string `json:"paths,omitempty"`
		SQL        string   `json:"sql,omitempty"`
	} `json:"data"`
	Input json.RawMessage `json:"input"`
}

func runDefinition(ctx context.Context, c config.Config, s *store.Store, j *jobs.Engine, raw string) (string, error) {
	var def jobDefinition
	if err := hub.DecodeStrict([]byte(raw), &def); err != nil {
		return "", err
	}
	if len(def.Input) == 0 {
		def.Input = json.RawMessage(`{}`)
	}
	if def.Data.Kind == "segments" {
		return j.SubmitSegments(ctx, def.Flow, def.Input, def.Data.SegmentIDs)
	}
	if def.Data.Kind == "raw" {
		return j.Submit(ctx, def.Flow, def.Input)
	}
	paths := []string{}
	for _, path := range def.Data.Paths {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if def.Data.Kind == "media" || def.Data.Kind == "sql" {
		in := selectionRequest{Mode: "media", forJobs: true}
		if def.Data.Kind == "sql" {
			if strings.TrimSpace(def.Data.SQL) == "" {
				return "", fmt.Errorf("SQL is required")
			}
			in.SQL = def.Data.SQL
		} else {
			if len(def.Data.MediaIDs) == 0 {
				return "", fmt.Errorf("select at least one file")
			}
			in.Filters = &selectionFilters{MediaIDs: def.Data.MediaIDs}
		}
		items, err := selectClips(ctx, s, c.SQLitePath, in)
		if err != nil {
			return "", err
		}
		seen := map[int64]bool{}
		paths = []string{}
		for _, clip := range items {
			if !seen[clip.Media.ID] {
				paths = append(paths, clip.Media.FilePath)
				seen[clip.Media.ID] = true
			}
		}
	} else if def.Data.Kind != "paths" {
		return "", fmt.Errorf("data kind must be media, paths, sql, segments, or raw")
	}
	return j.SubmitPaths(ctx, def.Flow, def.Input, paths)
}

func draftAPI(mux *http.ServeMux, c config.Config, s *store.Store, j *jobs.Engine) {
	mux.HandleFunc("GET /api/v1/drafts/{kind}", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.Documents(r.Context(), r.PathValue("kind"))
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /api/v1/drafts/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Document(r.Context(), r.PathValue("kind"), r.PathValue("id"))
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, d)
	})
	for _, method := range []string{"POST", "PUT"} {
		pattern := method + " /api/v1/drafts/{kind}"
		if method == "PUT" {
			pattern += "/{id}"
		}
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Name     string `json:"name"`
				Document string `json:"document"`
			}
			if err := decodeBody(w, r, &in); err != nil {
				workspaceError(w, err)
				return
			}
			d, err := s.SaveDocument(r.Context(), r.PathValue("kind"), store.Document{ID: r.PathValue("id"), Name: in.Name, Document: in.Document})
			if err != nil {
				workspaceError(w, err)
				return
			}
			jsonResponse(w, 200, d)
		})
	}
	mux.HandleFunc("DELETE /api/v1/drafts/{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DeleteDocument(r.Context(), r.PathValue("kind"), r.PathValue("id")); err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"removed": true})
	})
	mux.HandleFunc("POST /api/v1/drafts/jobs/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Document(r.Context(), "jobs", r.PathValue("id"))
		if err != nil {
			workspaceError(w, err)
			return
		}
		id, err := runDefinition(r.Context(), c, s, j, d.Document)
		if err != nil {
			workspaceError(w, err)
			return
		}
		w.Header().Set("Location", "/api/v1/jobs/"+id)
		jsonResponse(w, 202, map[string]string{"id": id})
	})
	mux.HandleFunc("POST /api/v1/manifests/validate", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Kind     string          `json:"kind"`
			Manifest json.RawMessage `json:"manifest"`
		}
		if err := decodeBody(w, r, &in); err != nil {
			workspaceError(w, err)
			return
		}
		record, err := hub.Decode(hub.WireKind(in.Kind), in.Manifest)
		if err != nil {
			workspaceError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"manifest": record.Value, "dependencies": record.Dependencies})
	})
}

func moduleAPI(mux *http.ServeMux, c config.Config, s *store.Store) {
	client := hub.Client{URL: c.HubURL}
	installer := func() modules.Installer {
		root := c.ModulesPath
		if !filepath.IsAbs(root) {
			root = filepath.Join(filepath.Dir(c.Path), root)
		}
		root, _ = filepath.Abs(root)
		return modules.Installer{Store: s, Root: root, Hub: client}
	}
	kindOf := func(r *http.Request) (string, error) {
		kind := hub.WireKind(r.PathValue("kind"))
		if !hub.ValidKind(kind) {
			return "", fmt.Errorf("kind must be tagtypes, flows, or programs")
		}
		return kind, nil
	}
	mux.HandleFunc("GET /api/v1/modules/{kind}", func(w http.ResponseWriter, r *http.Request) {
		kind, err := kindOf(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		if kind != hub.TagTypes {
			items, err := s.Modules(r.Context(), kind)
			if err != nil {
				apiError(w, 500, err)
				return
			}
			jsonResponse(w, 200, map[string]any{"items": items})
			return
		}
		rows, err := s.DB.QueryContext(r.Context(), "SELECT id,key,version,manifest,source FROM tagtype ORDER BY key")
		if err != nil {
			apiError(w, 500, err)
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id int64
			var key, raw, source string
			var version int
			if err = rows.Scan(&id, &key, &version, &raw, &source); err != nil {
				apiError(w, 500, err)
				return
			}
			items = append(items, map[string]any{"id": id, "key": key, "version": version, "manifest": json.RawMessage(raw), "source": source})
		}
		if err = rows.Err(); err != nil {
			apiError(w, 500, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": items})
	})
	mux.HandleFunc("GET /api/v1/hub/{kind}", func(w http.ResponseWriter, r *http.Request) {
		kind, err := kindOf(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		limit, offset, err := pagination(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		page, err := client.List(r.Context(), kind, "", limit, offset)
		if err != nil {
			apiError(w, 502, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("GET /api/v1/hub/{kind}/{key}/versions", func(w http.ResponseWriter, r *http.Request) {
		kind, err := kindOf(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		limit, offset, err := pagination(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		page, err := client.List(r.Context(), kind, r.PathValue("key"), limit, offset)
		if err != nil {
			apiError(w, 502, err)
			return
		}
		jsonResponse(w, 200, page)
	})
	mux.HandleFunc("POST /api/v1/modules/{kind}/resolve", func(w http.ResponseWriter, r *http.Request) {
		kind, err := kindOf(r)
		if err != nil {
			workspaceError(w, err)
			return
		}
		var in struct {
			Ref string `json:"ref"`
		}
		if err = decodeBody(w, r, &in); err != nil {
			workspaceError(w, err)
			return
		}
		ref, err := hub.ParseRef(kind, in.Ref)
		if err != nil {
			workspaceError(w, err)
			return
		}
		plan, err := client.Resolve(r.Context(), ref, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			apiError(w, 422, err)
			return
		}
		jsonResponse(w, 200, hub.ClientView(plan))
	})
	for _, action := range []string{"install", "update"} {
		mux.HandleFunc("POST /api/v1/modules/{kind}/"+action, func(w http.ResponseWriter, r *http.Request) {
			kind, err := kindOf(r)
			if err != nil {
				workspaceError(w, err)
				return
			}
			var in struct {
				Ref string `json:"ref"`
			}
			if err = decodeBody(w, r, &in); err != nil {
				workspaceError(w, err)
				return
			}
			ref, err := hub.ParseRef(kind, in.Ref)
			if err != nil {
				workspaceError(w, err)
				return
			}
			i := installer()
			var plan hub.Resolution
			if action == "update" {
				plan, err = i.Update(r.Context(), ref)
			} else {
				plan, err = i.Install(r.Context(), ref)
			}
			if err != nil {
				apiError(w, 409, err)
				return
			}
			jsonResponse(w, 200, hub.ClientView(plan))
		})
	}
	mux.HandleFunc("DELETE /api/v1/modules/{kind}/{key}", func(w http.ResponseWriter, r *http.Request) {
		kind, err := kindOf(r)
		if err == nil {
			if !hub.ValidIdentifier(r.PathValue("key")) {
				err = fmt.Errorf("invalid key")
			} else {
				err = installer().Remove(r.Context(), kind, r.PathValue("key"))
			}
		}
		if err != nil {
			apiError(w, 409, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"removed": true})
	})
}
