// Package web serves the page and its JSON API.
package web

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/okteto/users-cleanup/app/cleanup"
	"github.com/okteto/users-cleanup/app/service"
)

//go:embed index.html
var indexHTML []byte

// favicon is the icon of the okteto-community organization.
//
//go:embed favicon.png
var favicon []byte

// actionHeader must be present in every request that changes something. A page
// in another site cannot send it, which protects the admin's session from
// cross-site requests.
const actionHeader = "X-Users-Cleanup"

// Info is the static information shown in the page.
type Info struct {
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	InactiveDays int    `json:"inactiveDays"`
	Demo         bool   `json:"demo"`
}

// Server is the HTTP server of the service.
type Server struct {
	Manager *service.Manager
	Info    Info
	Logger  *slog.Logger
}

// Handler returns the routes of the service.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /favicon.png", s.favicon)
	mux.HandleFunc("GET /favicon.ico", s.favicon)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/plan", s.plan)
	mux.HandleFunc("GET /api/runs/{id}", s.run)
	mux.HandleFunc("POST /api/scan", s.guard(s.startScan))
	mux.HandleFunc("POST /api/run", s.guard(s.startRun))
	mux.HandleFunc("POST /api/schedule", s.guard(s.setSchedule))
	return mux
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(indexHTML)
}

func (s *Server) favicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(favicon)
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	s.json(w, http.StatusOK, struct {
		Info Info `json:"info"`
		service.Status
	}{Info: s.Info, Status: s.Manager.Status()})
}

func (s *Server) plan(w http.ResponseWriter, _ *http.Request) {
	s.json(w, http.StatusOK, s.Manager.Plan())
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, err := s.Manager.Run(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if run == nil {
		s.fail(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	s.json(w, http.StatusOK, run)
}

func (s *Server) startScan(w http.ResponseWriter, _ *http.Request) {
	s.started(w, s.Manager.StartScan())
}

func (s *Server) startRun(w http.ResponseWriter, _ *http.Request) {
	s.started(w, s.Manager.StartRun(cleanup.TriggerManual))
}

func (s *Server) started(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrBusy):
		s.fail(w, http.StatusConflict, err)
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Server) setSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
		Weekday int  `json:"weekday"`
		Hour    int  `json:"hour"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid request"))
		return
	}
	if err := s.Manager.SetSchedule(r.Context(), body.Enabled, body.Weekday, body.Hour); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.json(w, http.StatusOK, s.Manager.Status().Schedule)
}

// guard rejects the requests that do not come from the page itself.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(actionHeader) == "" {
			s.fail(w, http.StatusForbidden, errors.New("missing "+actionHeader+" header"))
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				s.fail(w, http.StatusForbidden, errors.New("cross-site requests are not allowed"))
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.Logger.Error("could not write the response", "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	s.json(w, code, map[string]string{"error": err.Error()})
}
