package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dbaasv1 "github.com/wso2/open-cloud-datacenter/dbaas/api/v1alpha1"
	"github.com/wso2/open-cloud-datacenter/dbaas/internal/tenants"
)

// Server holds shared state used by every request handler.
// Adding new dependencies (OIDC validator, quota enforcer) means
// adding a field here rather than threading parameters through every function.
type Server struct {
	k8sClient client.Client
	tenants   *tenants.Config
}

// RunGateway starts the HTTP gateway. It reads DBAAS_TENANTS_CONFIG for the
// path to tenants.yaml and refuses to start if the file is missing or invalid.
func RunGateway(addr string, k8sClient client.Client) error {
	tenantsPath := os.Getenv("DBAAS_TENANTS_CONFIG")
	if tenantsPath == "" {
		tenantsPath = "/etc/dbaas/tenants.yaml"
	}

	cfg, err := tenants.Load(tenantsPath)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}

	srv := &Server{
		k8sClient: k8sClient,
		tenants:   cfg,
	}

	auth := newAuthMiddleware(k8sClient, "dbaas-api-keys", "dbaas-system")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/dbinstances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			srv.handleListInstances(w, r)
		case http.MethodPost:
			srv.handleCreateInstance(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
	mux.HandleFunc("/dbinstances/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleInstanceRoute(w, r)
	})

	return http.ListenAndServe(addr, auth.Wrap(mux))
}

// defaultNamespace returns the fallback namespace when no tenant context is
// available yet. Replaced by tenants.Lookup() in Task 5 (Asgardeo auth).
func defaultNamespace() string {
	if ns := os.Getenv("DBAAS_DEFAULT_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	if !RequireRole(w, r, RoleAdmin, RoleOperator, RoleViewer) {
		return
	}
	var instances dbaasv1.DBInstanceList
	if err := s.k8sClient.List(r.Context(), &instances); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instances)
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	if !RequireRole(w, r, RoleAdmin) {
		return
	}
	defer r.Body.Close()

	var instance dbaasv1.DBInstance
	if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if instance.Name == "" {
		writeError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}
	if instance.APIVersion == "" {
		instance.APIVersion = dbaasv1.GroupVersion.String()
	}
	if instance.Kind == "" {
		instance.Kind = "DBInstance"
	}
	if instance.Namespace == "" {
		instance.Namespace = defaultNamespace()
	}
	if err := s.k8sClient.Create(r.Context(), &instance); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

func (s *Server) handleInstanceRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/dbinstances/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "instance name is required")
		return
	}

	name := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleGetInstance(w, r, name)
		case http.MethodDelete:
			s.handleDeleteInstance(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	switch parts[1] {
	case "start":
		s.handleSetRunning(w, r, name, true)
	case "stop":
		s.handleSetRunning(w, r, name, false)
	default:
		writeError(w, http.StatusNotFound, "unsupported action")
	}
}

func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request, name string) {
	if !RequireRole(w, r, RoleAdmin, RoleOperator, RoleViewer) {
		return
	}
	var instance dbaasv1.DBInstance
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instance)
}

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, name string) {
	if !RequireRole(w, r, RoleAdmin) {
		return
	}
	instance := &dbaasv1.DBInstance{}
	instance.Name = name
	instance.Namespace = defaultNamespace()
	instance.APIVersion = dbaasv1.GroupVersion.String()
	instance.Kind = "DBInstance"

	if err := s.k8sClient.Delete(r.Context(), instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deletion requested", "name": name})
}

func (s *Server) handleSetRunning(w http.ResponseWriter, r *http.Request, name string, running bool) {
	if !RequireRole(w, r, RoleAdmin, RoleOperator) {
		return
	}
	var instance dbaasv1.DBInstance
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Namespace: defaultNamespace(), Name: name}, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	instance.Spec.Running = boolPtr(running)
	if err := s.k8sClient.Update(r.Context(), &instance); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, instance)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func boolPtr(v bool) *bool {
	return &v
}
