// Package adminapi serves the project state/recovery operations over HTTP. It is
// the single internal entry point shared by the `shoka project` CLI subcommands
// and the Web UI recovery dialog (directive §9). It is mounted under /api/ on the
// Web UI listener behind the same auth middleware.
package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/utils"
)

// Store is the subset of storage the admin API needs.
type Store interface {
	State(namespace, projectName string) storage.ProjectState
	DetectDrift(namespace, projectName string) (storage.DriftSummary, error)
	RecoverProject(namespace, projectName string, mode storage.RecoveryMode) error
}

type statusResponse struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	State     string `json:"state"`
}

type driftResponse struct {
	Namespace string   `json:"namespace"`
	Project   string   `json:"project"`
	State     string   `json:"state"`
	Added     []string `json:"added"`
	Modified  []string `json:"modified"`
	Deleted   []string `json:"deleted"`
}

type recoverResponse struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	State     string `json:"state"`
	Message   string `json:"message"`
}

// New returns an http.Handler serving the admin endpoints:
//
//	GET  /api/project/status   ?namespace=&project=
//	POST /api/project/rescan   ?namespace=&project=
//	POST /api/project/recover  ?namespace=&project=&mode=accept-working-tree|accept-head
func New(s Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/project/status", func(w http.ResponseWriter, r *http.Request) {
		ns, project, ok := nsProject(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, statusResponse{Namespace: ns, Project: project, State: string(s.State(ns, project))})
	})

	mux.HandleFunc("/api/project/rescan", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		ns, project, ok := nsProject(w, r)
		if !ok {
			return
		}
		sum, err := s.DetectDrift(ns, project)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, driftResponse{
			Namespace: ns, Project: project, State: string(sum.State),
			Added: sum.Added, Modified: sum.Modified, Deleted: sum.Deleted,
		})
	})

	mux.HandleFunc("/api/project/recover", func(w http.ResponseWriter, r *http.Request) {
		if !requirePost(w, r) {
			return
		}
		ns, project, ok := nsProject(w, r)
		if !ok {
			return
		}
		mode := storage.RecoveryMode(r.URL.Query().Get("mode"))
		if mode != storage.RecoverAcceptWorkingTree && mode != storage.RecoverAcceptHead {
			httpError(w, http.StatusBadRequest, "mode must be accept-working-tree or accept-head")
			return
		}
		if err := s.RecoverProject(ns, project, mode); err != nil {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, recoverResponse{
			Namespace: ns, Project: project, State: string(s.State(ns, project)),
			Message: "recovered with " + string(mode),
		})
	})

	return mux
}

func nsProject(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = "default"
	}
	project := r.URL.Query().Get("project")
	if project == "" {
		httpError(w, http.StatusBadRequest, "project is required")
		return "", "", false
	}
	if !utils.IsValidName(ns) || !utils.IsValidName(project) {
		httpError(w, http.StatusBadRequest, "invalid namespace or project")
		return "", "", false
	}
	return ns, project, true
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		httpError(w, http.StatusMethodNotAllowed, "use POST")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
