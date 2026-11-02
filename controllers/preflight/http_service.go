package preflight

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"gopkg.in/yaml.v2"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// preflightHTTPPort is the port the HTTP service listens on.
	preflightHTTPPort = ":8443"

	// requestIDLabel is placed on internal UpgradePreflightCheck CRs so the HTTP
	// service can find them by requestId without an in-memory map.
	requestIDLabel = "preflight.managed.openshift.io/request-id"

	// tlsCertEnv / tlsKeyEnv allow overriding the cert paths (e.g. for local dev).
	tlsCertEnv      = "PREFLIGHT_TLS_CERT"
	tlsKeyEnv       = "PREFLIGHT_TLS_KEY"
	defaultCertFile = "/etc/preflight-tls/tls.crt"
	defaultKeyFile  = "/etc/preflight-tls/tls.key"
)

// CheckRequest is the body of POST /api/v1/preflight-checks.
type CheckRequest struct {
	TargetVersion       string   `json:"targetVersion"`
	TargetChannel       string   `json:"targetChannel"`
	Platform            string   `json:"platform"`
	CheckTimeoutSeconds int32    `json:"checkTimeoutSeconds,omitempty"`
	ChecksToRun         []string `json:"checksToRun,omitempty"`
	Description         string   `json:"description,omitempty"`
}

// CheckResponse is the body returned by GET /api/v1/preflight-checks/{id}.
type CheckResponse struct {
	RequestID       string        `json:"requestId"`
	Status          string        `json:"status"`
	StartedAt       *metav1.Time  `json:"startedAt,omitempty"`
	CompletedAt     *metav1.Time  `json:"completedAt,omitempty"`
	TargetVersion   string        `json:"targetVersion"`
	TargetChannel   string        `json:"targetChannel"`
	Platform        string        `json:"platform"`
	ChecksTotal     int           `json:"checksTotal"`
	ChecksCompleted int           `json:"checksCompleted"`
	ChecksPassed    int           `json:"checksPassed"`
	ChecksFailed    int           `json:"checksFailed"`
	ChecksSkipped   int           `json:"checksSkipped"`
	ChecksErrored   int           `json:"checksErrored"`
	OverallStatus   string        `json:"overallStatus,omitempty"`
	Message         string        `json:"message,omitempty"`
	Results         []CheckResult `json:"results,omitempty"`
}

// CheckResult is a per-check entry within CheckResponse.
type CheckResult struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Message string   `json:"message,omitempty"`
	Details []string `json:"details,omitempty"`
}

// PreflightHTTPServer serves the preflight check HTTP API.
// It uses the operator's own service-account client to create internal
// UpgradePreflightCheck CRs (which the sre-regular-user-validation webhook
// permits for the operator SA), while enforcing the caller's RBAC via
// TokenReview + SubjectAccessReview before acting on their behalf.
type PreflightHTTPServer struct {
	Client    client.Client
	Namespace string
	Logger    logr.Logger
}

// Start listens on preflightHTTPPort with TLS until ctx is cancelled.
// If TLS certificates are not present the server logs a warning and returns nil
// (non-fatal: the operator continues without HTTP access to preflight checks).
func (s *PreflightHTTPServer) Start(ctx context.Context) error {
	certFile := envOr(tlsCertEnv, defaultCertFile)
	keyFile := envOr(tlsKeyEnv, defaultKeyFile)

	if _, err := os.Stat(certFile); err != nil {
		s.Logger.Info("Preflight HTTP service: TLS cert not found, HTTP service disabled",
			"cert", certFile, "hint", "annotate the Service with service.beta.openshift.io/serving-cert-secret-name")
		return err
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("preflight HTTP: loading TLS cert: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/preflight-checks", s.route)
	mux.HandleFunc("/api/v1/preflight-checks/", s.route)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:              preflightHTTPPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	//nolint:gosec,contextcheck // shutdown timeout is independent of request context
	go func(stopCtx context.Context) {
		<-stopCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}(ctx)

	s.Logger.Info("Starting preflight HTTP service", "addr", preflightHTTPPort)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("preflight HTTP server: %w", err)
	}
	return nil
}

// route dispatches based on method and path.
func (s *PreflightHTTPServer) route(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "Authorization: Bearer <token> required", http.StatusUnauthorized)
		return
	}

	// /api/v1/preflight-checks/{id} has 5 slash-separated parts after trim
	path := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	requestID := ""
	if len(parts) == 5 {
		requestID = parts[4]
	}

	switch {
	case r.Method == http.MethodPost && requestID == "":
		s.handleCreate(w, r, token)
	case r.Method == http.MethodGet && requestID != "":
		s.handleGet(w, r, token, requestID)
	case r.Method == http.MethodGet && requestID == "":
		s.handleList(w, r, token)
	case r.Method == http.MethodDelete && requestID != "":
		s.handleDelete(w, r, token, requestID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *PreflightHTTPServer) handleCreate(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()
	if err := s.authorizeToken(ctx, token, "create"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	var req CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCheckRequest(req); err != nil {
		http.Error(w, "Bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Clean up expired checks before enforcing limit (opportunistic cleanup)
	s.cleanupExpiredChecks(ctx)

	// Enforce limit: max 2 active preflight checks at a time
	if err := s.checkActiveLimit(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}

	requestID := uuid.New().String()
	cr := s.buildInternalCR(requestID, req)
	if err := s.Client.Create(ctx, cr); err != nil {
		s.Logger.Error(err, "Failed to create UpgradePreflightCheck", "requestId", requestID)
		http.Error(w, "Internal error creating preflight check", http.StatusInternalServerError)
		return
	}

	s.Logger.Info("Preflight check created via HTTP", "requestId", requestID, "crName", cr.Name)

	// Warn if feature gate is not enabled (checks won't run)
	s.warnIfFeatureGateDisabled(ctx, requestID)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"requestId": requestID,
		"status":    "accepted",
		"statusUrl": fmt.Sprintf("/api/v1/preflight-checks/%s", requestID),
		"message":   "Preflight check accepted",
	})
}

func (s *PreflightHTTPServer) handleGet(w http.ResponseWriter, r *http.Request, token, requestID string) {
	ctx := r.Context()
	if err := s.authorizeToken(ctx, token, "get"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}
	cr, err := s.findCR(ctx, requestID)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, buildResponse(requestID, cr))
}

func (s *PreflightHTTPServer) handleList(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()
	if err := s.authorizeToken(ctx, token, "list"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	list := &upgradev1alpha1.UpgradePreflightCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.HasLabels{requestIDLabel},
	); err != nil {
		http.Error(w, "Internal error listing checks", http.StatusInternalServerError)
		return
	}

	responses := make([]CheckResponse, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		rid := cr.Labels[requestIDLabel]
		responses = append(responses, buildResponse(rid, cr))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requests": responses,
		"total":    len(responses),
	})
}

func (s *PreflightHTTPServer) handleDelete(w http.ResponseWriter, r *http.Request, token, requestID string) {
	ctx := r.Context()
	if err := s.authorizeToken(ctx, token, "delete"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}
	cr, err := s.findCR(ctx, requestID)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if err := s.Client.Delete(ctx, cr); err != nil {
		s.Logger.Error(err, "Failed to delete UpgradePreflightCheck", "requestId", requestID)
		http.Error(w, "Internal error deleting preflight check", http.StatusInternalServerError)
		return
	}
	s.Logger.Info("Preflight check deleted via HTTP", "requestId", requestID, "crName", cr.Name)
	w.WriteHeader(http.StatusNoContent)
}

// warnIfFeatureGateDisabled checks if the PreflightController feature gate is enabled
// and logs a warning if not, helping users debug why their checks aren't running.
func (s *PreflightHTTPServer) warnIfFeatureGateDisabled(ctx context.Context, requestID string) {
	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, client.ObjectKey{
		Namespace: s.Namespace,
		Name:      "managed-upgrade-operator-config",
	}, cm); err != nil {
		// ConfigMap not found or error - skip check (don't block request)
		return
	}

	configYAML, ok := cm.Data["config.yaml"]
	if !ok {
		return
	}

	var config struct {
		FeatureGate struct {
			Enabled []string `yaml:"enabled"`
		} `yaml:"featureGate"`
	}

	if err := yaml.Unmarshal([]byte(configYAML), &config); err != nil {
		return
	}

	// Check if PreflightController is enabled
	for _, f := range config.FeatureGate.Enabled {
		if f == "PreflightController" {
			return // Feature gate enabled, all good
		}
	}

	// Feature gate not enabled - warn user
	s.Logger.Info("PreflightController feature gate not enabled - check will remain pending until enabled",
		"requestId", requestID,
		"help", "Add 'PreflightController' to featureGate.enabled in managed-upgrade-operator-config ConfigMap and restart the operator")
}

// cleanupExpiredChecks deletes preflight checks that have passed their TTL.
// This runs opportunistically when new requests come in to avoid resource buildup.
func (s *PreflightHTTPServer) cleanupExpiredChecks(ctx context.Context) {
	list := &upgradev1alpha1.UpgradePreflightCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.HasLabels{requestIDLabel},
	); err != nil {
		s.Logger.Error(err, "Failed to list checks for cleanup")
		return
	}

	now := time.Now()
	for i := range list.Items {
		cr := &list.Items[i]
		deleteAfter := cr.Annotations["preflight.managed.openshift.io/delete-after"]
		if deleteAfter == "" {
			continue
		}
		expiryTime, err := time.Parse(time.RFC3339, deleteAfter)
		if err != nil {
			s.Logger.Error(err, "Invalid delete-after annotation", "crName", cr.Name, "deleteAfter", deleteAfter)
			continue
		}
		if now.After(expiryTime) {
			if err := s.Client.Delete(ctx, cr); err != nil {
				s.Logger.Error(err, "Failed to delete expired check", "crName", cr.Name)
			} else {
				s.Logger.Info("Deleted expired preflight check", "crName", cr.Name, "expiry", deleteAfter)
			}
		}
	}
}

// checkActiveLimit enforces a maximum of 2 active preflight checks to prevent resource exhaustion.
// Returns an error with instructions if the limit is reached.
func (s *PreflightHTTPServer) checkActiveLimit(ctx context.Context) error {
	const maxActiveChecks = 2
	list := &upgradev1alpha1.UpgradePreflightCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.HasLabels{requestIDLabel},
	); err != nil {
		return fmt.Errorf("internal error checking active checks: %w", err)
	}
	if len(list.Items) >= maxActiveChecks {
		return fmt.Errorf("too many active preflight checks (%d/%d). List existing checks with GET /api/v1/preflight-checks and delete completed ones with DELETE /api/v1/preflight-checks/{id} before creating new checks", len(list.Items), maxActiveChecks)
	}
	return nil
}

// authorizeToken performs TokenReview (authn) then SubjectAccessReview (authz).
// verb should be "create", "get", "list", or "delete".
func (s *PreflightHTTPServer) authorizeToken(ctx context.Context, token, verb string) error {
	tr := &authnv1.TokenReview{Spec: authnv1.TokenReviewSpec{Token: token}}
	if err := s.Client.Create(ctx, tr); err != nil {
		return fmt.Errorf("token review: %w", err)
	}
	if !tr.Status.Authenticated {
		return fmt.Errorf("unauthenticated")
	}

	extra := make(map[string]authzv1.ExtraValue, len(tr.Status.User.Extra))
	for k, v := range tr.Status.User.Extra {
		extra[k] = authzv1.ExtraValue(v)
	}

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   tr.Status.User.Username,
			Groups: tr.Status.User.Groups,
			Extra:  extra,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: s.Namespace,
				Verb:      verb,
				Group:     "upgrade.managed.openshift.io",
				Resource:  "upgradepreflightchecks",
			},
		},
	}
	if err := s.Client.Create(ctx, sar); err != nil {
		return fmt.Errorf("subject access review: %w", err)
	}
	if !sar.Status.Allowed {
		reason := sar.Status.Reason
		if reason == "" {
			reason = "not allowed"
		}
		return fmt.Errorf("%s", reason)
	}
	return nil
}

func (s *PreflightHTTPServer) findCR(ctx context.Context, requestID string) (*upgradev1alpha1.UpgradePreflightCheck, error) {
	list := &upgradev1alpha1.UpgradePreflightCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.MatchingLabels{requestIDLabel: requestID},
	); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("not found")
	}
	return &list.Items[0], nil
}

func (s *PreflightHTTPServer) buildInternalCR(requestID string, req CheckRequest) *upgradev1alpha1.UpgradePreflightCheck {
	annotations := map[string]string{
		// Auto-delete after 24 hours to prevent resource buildup
		"preflight.managed.openshift.io/delete-after": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	if req.Description != "" {
		annotations["preflight.managed.openshift.io/description"] = req.Description
	}
	return &upgradev1alpha1.UpgradePreflightCheck{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "http-",
			Namespace:    s.Namespace,
			Labels:       map[string]string{requestIDLabel: requestID},
			Annotations:  annotations,
		},
		Spec: upgradev1alpha1.UpgradePreflightCheckSpec{
			UpgradeConfigSpec: upgradev1alpha1.UpgradeConfigSpec{
				Type:                 upgradev1alpha1.UpgradeType(req.Platform),
				Desired:              upgradev1alpha1.Update{Version: req.TargetVersion, Channel: req.TargetChannel},
				UpgradeAt:            time.Now().Format(time.RFC3339),
				PDBForceDrainTimeout: 60,
			},
			CheckTimeoutSeconds: req.CheckTimeoutSeconds,
			Checks:              req.ChecksToRun,
		},
	}
}

func buildResponse(requestID string, cr *upgradev1alpha1.UpgradePreflightCheck) CheckResponse {
	status := "pending"
	if cr.Status.StartedAt != nil {
		status = "running"
	}
	if cr.Status.CompletedAt != nil {
		status = "completed"
	}

	resp := CheckResponse{
		RequestID:     requestID,
		Status:        status,
		StartedAt:     cr.Status.StartedAt,
		CompletedAt:   cr.Status.CompletedAt,
		TargetVersion: cr.Spec.Desired.Version,
		TargetChannel: cr.Spec.Desired.Channel,
		Platform:      string(cr.Spec.Type),
		ChecksTotal:   len(cr.Status.Checks),
	}

	anyBad := false
	for _, c := range cr.Status.Checks {
		r := CheckResult{Name: c.Name}
		switch c.Execution.State {
		case upgradev1alpha1.CheckCompleted:
			resp.ChecksCompleted++
			if c.Result != nil {
				r.Status = strings.ToLower(string(c.Result.Verdict))
				r.Message = c.Result.Message
				r.Details = c.Result.Details
				if c.Result.Verdict == upgradev1alpha1.CheckFailed {
					resp.ChecksFailed++
					anyBad = true
				} else {
					resp.ChecksPassed++
				}
			}
		case upgradev1alpha1.CheckError:
			resp.ChecksErrored++
			anyBad = true
			r.Status = "error"
			r.Message = c.Execution.Error
		case upgradev1alpha1.CheckSkipped:
			resp.ChecksSkipped++
			r.Status = "skipped"
		}
		resp.Results = append(resp.Results, r)
	}

	if cr.Status.CompletedAt != nil {
		if anyBad {
			resp.OverallStatus = "failed"
			resp.Message = fmt.Sprintf("Upgrade not recommended — %d check(s) failed, %d errored",
				resp.ChecksFailed, resp.ChecksErrored)
		} else {
			resp.OverallStatus = "passed"
			resp.Message = "Cluster is ready for upgrade"
		}
	}
	return resp
}

func validateCheckRequest(req CheckRequest) error {
	if req.TargetVersion == "" {
		return fmt.Errorf("targetVersion is required")
	}
	if req.TargetChannel == "" {
		return fmt.Errorf("targetChannel is required")
	}
	if req.Platform != "OSD" && req.Platform != "ARO" {
		return fmt.Errorf("platform must be OSD or ARO, got %q", req.Platform)
	}
	return nil
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
