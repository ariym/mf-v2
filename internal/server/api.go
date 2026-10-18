package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/store"
)

func jsonResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func apiError(w http.ResponseWriter, status int, e error) {
	jsonResponse(w, status, map[string]string{"error": e.Error()})
}
func API(c config.Config, s *store.Store, j *jobs.Engine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := s.Ping(r.Context()); e != nil {
			apiError(w, 503, e)
			return
		}
		jsonResponse(w, 200, map[string]any{"ready": true, "app_name": c.AppName, "version": config.Version})
	})
	mux.HandleFunc("GET /api/v1/media", func(w http.ResponseWriter, r *http.Request) {
		limit, offset := 100, 0
		var e error
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, e = strconv.Atoi(v)
		}
		if e != nil || limit < 1 || limit > 1000 {
			apiError(w, 400, errors.New("limit must be between 1 and 1000"))
			return
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			offset, e = strconv.Atoi(v)
		}
		if e != nil || offset < 0 {
			apiError(w, 400, errors.New("offset must be nonnegative"))
			return
		}
		data, e := s.List(r.Context(), limit, offset)
		if e != nil {
			apiError(w, 500, e)
			return
		}
		jsonResponse(w, 200, map[string]any{"items": data, "limit": limit, "offset": offset})
	})
	mux.HandleFunc("GET /api/v1/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 {
			apiError(w, 400, errors.New("invalid media ID"))
			return
		}
		m, e := s.Get(r.Context(), id)
		if errors.Is(e, sql.ErrNoRows) {
			apiError(w, 404, errors.New("media not found"))
			return
		}
		if e != nil {
			apiError(w, 500, e)
			return
		}
		jsonResponse(w, 200, m)
	})
	mux.HandleFunc("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Flow  string          `json:"flow"`
			Input json.RawMessage `json:"input"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if e := d.Decode(&body); e != nil {
			apiError(w, 400, e)
			return
		}
		var extra any
		if e := d.Decode(&extra); e != io.EOF {
			apiError(w, 400, errors.New("expected one JSON object"))
			return
		}
		id, e := j.Submit(r.Context(), body.Flow, body.Input)
		if e != nil {
			var input *jobs.InputError
			status := 500
			if errors.As(e, &input) {
				status = 400
			}
			if errors.Is(e, jobs.ErrStopping) {
				status = 503
			}
			apiError(w, status, e)
			return
		}
		w.Header().Set("Location", "/api/v1/jobs/"+id)
		jsonResponse(w, 202, map[string]string{"id": id})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		job, e := j.Get(r.Context(), r.PathValue("id"))
		if errors.Is(e, sql.ErrNoRows) {
			apiError(w, 404, errors.New("job not found"))
			return
		}
		if e != nil {
			apiError(w, 500, e)
			return
		}
		jsonResponse(w, 200, job)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiError(w, 404, errors.New("endpoint not found")) })
	return mux
}
