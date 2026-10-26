package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ariym/mf-v2/internal/config"
	"github.com/ariym/mf-v2/internal/hub"
	"github.com/ariym/mf-v2/internal/jobs"
	"github.com/ariym/mf-v2/internal/modules"
	"github.com/ariym/mf-v2/internal/store"
)

func requestBody(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
func requestAPI(mux *http.ServeMux, c config.Config, s *store.Store, e *jobs.Engine) {
	client := hub.Client{URL: c.HubURL}
	mux.HandleFunc("POST /api/v1/requests/plan", func(w http.ResponseWriter, r *http.Request) {
		var in jobs.RequestInput
		if err := requestBody(w, r, &in); err != nil {
			apiError(w, 400, err)
			return
		}
		p, err := e.PlanRequest(r.Context(), client, in, "")
		if err != nil {
			apiError(w, 400, err)
			return
		}
		jsonResponse(w, 200, p)
	})
	mux.HandleFunc("POST /api/v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var p jobs.RequestPlan
		if err := requestBody(w, r, &p); err != nil {
			apiError(w, 400, err)
			return
		}
		id, err := e.AcceptRequest(r.Context(), client, p)
		if err != nil {
			apiError(w, 409, err)
			return
		}
		w.Header().Set("Location", "/api/v1/requests/"+id)
		jsonResponse(w, 202, map[string]string{"id": id})
	})
	mux.HandleFunc("GET /api/v1/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := e.GetRequest(r.Context(), r.PathValue("id"))
		if err != nil {
			status := 500
			if errors.Is(err, sql.ErrNoRows) {
				status = 404
			}
			apiError(w, status, err)
			return
		}
		jsonResponse(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/requests/bootstrap/plan", func(w http.ResponseWriter, r *http.Request) {
		p, err := e.BootstrapProbePlan(r.Context(), client)
		if err != nil {
			apiError(w, 400, err)
			return
		}
		jsonResponse(w, 200, p)
	})
	mux.HandleFunc("POST /api/v1/requests/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		var approved jobs.RequestPlan
		if err := requestBody(w, r, &approved); err != nil {
			apiError(w, 400, err)
			return
		}
		fresh, err := e.BootstrapProbePlan(r.Context(), client)
		if err != nil {
			apiError(w, 400, err)
			return
		}
		fresh.ID = approved.ID
		fresh.Fingerprint = ""
		approved.Fingerprint = ""
		a, _ := json.Marshal(fresh)
		b, _ := json.Marshal(approved)
		if !store.SameJSON(a, b) {
			apiError(w, 409, errors.New("bootstrap plan changed; review again"))
			return
		}
		installer := modules.Installer{Store: s, Root: c.ModulesPath, Hub: client}
		for _, res := range fresh.Resolutions {
			if _, err = installer.InstallResolved(r.Context(), res); err != nil {
				apiError(w, 400, err)
				return
			}
		}
		jsonResponse(w, 200, map[string]bool{"installed": true})
	})
}
