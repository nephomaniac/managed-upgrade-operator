package upgradediags

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
	"github.com/openshift/managed-upgrade-operator/version"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// diagsHTTPPort is the port the HTTP service listens on.
	diagsHTTPPort = ":8443"

	// requestIDLabel is placed on internal UpgradeDiagsCheck CRs so the HTTP
	// service can find them by requestId without an in-memory map.
	requestIDLabel = "upgradediags.managed.openshift.io/request-id"

	// tlsCertEnv / tlsKeyEnv allow overriding the cert paths (e.g. for local dev).
	tlsCertEnv      = "UPGRADEDIAGS_TLS_CERT"
	tlsKeyEnv       = "UPGRADEDIAGS_TLS_KEY"
	defaultCertFile = "/etc/upgradediags-tls/tls.crt"
	defaultKeyFile  = "/etc/upgradediags-tls/tls.key"
)

// CheckRequest is the body of POST /api/v1/upgrade-diags.
type CheckRequest struct {
	TargetVersion       string   `json:"targetVersion"`
	TargetChannel       string   `json:"targetChannel"`
	Platform            string   `json:"platform"`
	DefaultCheckTimeoutSeconds int32    `json:"defaultCheckTimeoutSeconds,omitempty"`
	OverallTimeoutSeconds      int32    `json:"overallTimeoutSeconds,omitempty"`
	ChecksToRun                []string `json:"checksToRun,omitempty"`
	Description         string   `json:"description,omitempty"`
	// NamePrefix sets a custom prefix for the generated UpgradeDiagsCheck CR name.
	// Must be a valid DNS label segment (lowercase alphanumeric and hyphens, no leading/trailing hyphens).
	// Defaults to "upgradediags" if empty or invalid.
	NamePrefix string `json:"namePrefix,omitempty"`
}

// CheckResponse is the body returned by GET /api/v1/upgrade-diags/{id}.
type CheckResponse struct {
	RequestID        string        `json:"requestId"`
	Status           string        `json:"status"`
	ClusterInfraName string        `json:"clusterInfraName,omitempty"`
	APIServerURL     string        `json:"apiServerURL,omitempty"`
	StartedAt        *metav1.Time  `json:"startedAt,omitempty"`
	CompletedAt      *metav1.Time  `json:"completedAt,omitempty"`
	TargetVersion    string        `json:"targetVersion"`
	TargetChannel    string        `json:"targetChannel"`
	Platform         string        `json:"platform"`
	ChecksTotal     int           `json:"checksTotal"`
	ChecksCompleted int           `json:"checksCompleted"`
	ChecksPassed    int           `json:"checksPassed"`
	ChecksWarned    int           `json:"checksWarned"`
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
	Owner   string   `json:"owner,omitempty"`
	Action  string   `json:"action,omitempty"`
	Message string   `json:"message,omitempty"`
	Details []string `json:"details,omitempty"`
}

// PreflightHTTPServer serves the readiness check HTTP API.
// It uses the operator's own service-account client to create internal
// UpgradeDiagsCheck CRs (which the sre-regular-user-validation webhook
// permits for the operator SA), while enforcing the caller's RBAC via
// TokenReview + SubjectAccessReview before acting on their behalf.
type UpgradeDiagsHTTPServer struct {
	Client    client.Client
	Namespace string
	Logger    logr.Logger
}

// Start listens on diagsHTTPPort with TLS until ctx is cancelled.
// If TLS certificates are not present the server logs a warning and returns nil
// (non-fatal: the operator continues without HTTP access to readiness checks).
func (s *UpgradeDiagsHTTPServer) Start(ctx context.Context) error {
	certFile := envOr(tlsCertEnv, defaultCertFile)
	keyFile := envOr(tlsKeyEnv, defaultKeyFile)

	if _, err := os.Stat(certFile); err != nil {
		s.Logger.Info("Upgrade diags HTTP service: TLS cert not found, HTTP service disabled",
			"cert", certFile, "hint", "annotate the Service with service.beta.openshift.io/serving-cert-secret-name")
		return err
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("upgrade diags HTTP: loading TLS cert: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/upgrade-diags", s.route)
	mux.HandleFunc("/api/v1/upgrade-diags/", s.route)
	mux.HandleFunc("/api/v1/upgrade-status", s.routeStatus)
	mux.HandleFunc("/api/v1/upgrade-status/", s.routeStatus)
	mux.HandleFunc("/api/v1/available-versions", s.routeAvailableVersions)
	mux.HandleFunc("/api/v1/version", s.routeVersion)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{
		Addr:              diagsHTTPPort,
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

	s.Logger.Info("Starting upgrade diags HTTP service", "addr", diagsHTTPPort)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("upgrade diags HTTP server: %w", err)
	}
	return nil
}

// CatalogEntry describes a single registered readiness check for the catalog endpoint.
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       string `json:"owner"`
	TimeoutSecs int    `json:"timeoutSeconds,omitempty"`
}

// route dispatches based on method and path.
func (s *UpgradeDiagsHTTPServer) route(w http.ResponseWriter, r *http.Request) {
	// Catch any panic in a handler so a nil-pointer or unexpected error in this
	// controller cannot crash the net/http goroutine and take down the HTTP service.
	// Go's net/http recovers handler panics too, but logs them to stderr; this gives
	// us structured logging and a proper 500 response.
	defer func() {
		if p := recover(); p != nil {
			s.Logger.Error(fmt.Errorf("%v", p), "panic in upgrade diags HTTP handler; request aborted",
				"method", r.Method, "path", r.URL.Path)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()

	// /api/v1/upgrade-diags/{id} has 5 slash-separated parts after trim
	path := strings.TrimSuffix(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	requestID := ""
	if len(parts) == 5 {
		requestID = parts[4]
	}

	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "Authorization: Bearer <token> required", http.StatusUnauthorized)
		return
	}

	switch {
	case r.Method == http.MethodPost && requestID == "":
		s.handleCreate(w, r, token)
	case r.Method == http.MethodGet && requestID == "checks":
		s.handleCatalog(w, r, token)
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

// handleCatalog returns the list of registered checks with their names, descriptions,
// default owners, and timeouts.
func (s *UpgradeDiagsHTTPServer) handleCatalog(w http.ResponseWriter, r *http.Request, token string) {
	if err := s.authorizeToken(r.Context(), token, "list"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}
	entries := make([]CatalogEntry, 0, len(registeredChecks))
	for _, c := range registeredChecks {
		entry := CatalogEntry{
			Name:        c.Name(),
			Description: c.Description(),
			Owner:       string(c.DefaultOwner()),
		}
		if t := c.DefaultTimeout(); t > 0 {
			entry.TimeoutSecs = int(t.Seconds())
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"checks": entries,
		"total":  len(entries),
	})
}

func (s *UpgradeDiagsHTTPServer) handleCreate(w http.ResponseWriter, r *http.Request, token string) {
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

	// Enforce limit: max 2 active readiness checks at a time
	if err := s.checkActiveLimit(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}

	requestID := uuid.New().String()
	cr := s.buildInternalCR(requestID, req)
	if err := s.Client.Create(ctx, cr); err != nil {
		s.Logger.Error(err, "Failed to create UpgradeDiagsCheck", "requestId", requestID)
		http.Error(w, "Internal error creating upgrade diags check", http.StatusInternalServerError)
		return
	}

	s.Logger.Info("Upgrade diags check created via HTTP", "requestId", requestID, "crName", cr.Name)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"requestId": requestID,
		"status":    "accepted",
		"statusUrl": fmt.Sprintf("/api/v1/upgrade-diags/%s", requestID),
		"message":   "Upgrade diags check accepted",
	})
}

func (s *UpgradeDiagsHTTPServer) handleGet(w http.ResponseWriter, r *http.Request, token, requestID string) {
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
	resp := buildResponse(requestID, cr)
	s.fillClusterIdentifier(ctx, &resp)
	writeJSON(w, http.StatusOK, resp)
}

func (s *UpgradeDiagsHTTPServer) handleList(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()
	if err := s.authorizeToken(ctx, token, "list"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	list := &upgradev1alpha1.UpgradeDiagsCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.HasLabels{requestIDLabel},
	); err != nil {
		http.Error(w, "Internal error listing checks", http.StatusInternalServerError)
		return
	}

	// Fetch cluster identifier once and apply to all responses.
	infraName, apiServerURL := s.fetchClusterIdentifier(ctx)

	responses := make([]CheckResponse, 0, len(list.Items))
	for i := range list.Items {
		cr := &list.Items[i]
		rid := cr.Labels[requestIDLabel]
		resp := buildResponse(rid, cr)
		if resp.ClusterInfraName == "" {
			resp.ClusterInfraName = infraName
		}
		if resp.APIServerURL == "" {
			resp.APIServerURL = apiServerURL
		}
		responses = append(responses, resp)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requests": responses,
		"total":    len(responses),
	})
}

// fetchClusterIdentifier reads the cluster infrastructure name and API server URL
// directly from the Infrastructure CR. Called when the CR status fields are empty
// (e.g. when the CRD schema predates the cluster-identifier fields).
func (s *UpgradeDiagsHTTPServer) fetchClusterIdentifier(ctx context.Context) (infraName, apiServerURL string) {
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure",
	})
	if err := s.Client.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
		s.Logger.Error(err, "HTTP: failed to read Infrastructure CR for cluster identifier")
		return "", ""
	}
	infraName, _, _ = unstructured.NestedString(infra.Object, "status", "infrastructureName")
	apiServerURL, _, _ = unstructured.NestedString(infra.Object, "status", "apiServerURL")
	return infraName, apiServerURL
}

// fillClusterIdentifier populates ClusterInfraName and APIServerURL on resp when
// they are absent from the CR status (CRD schema not yet updated).
func (s *UpgradeDiagsHTTPServer) fillClusterIdentifier(ctx context.Context, resp *CheckResponse) {
	if resp.ClusterInfraName != "" && resp.APIServerURL != "" {
		return
	}
	infraName, apiServerURL := s.fetchClusterIdentifier(ctx)
	if resp.ClusterInfraName == "" {
		resp.ClusterInfraName = infraName
	}
	if resp.APIServerURL == "" {
		resp.APIServerURL = apiServerURL
	}
}

func (s *UpgradeDiagsHTTPServer) handleDelete(w http.ResponseWriter, r *http.Request, token, requestID string) {
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
		s.Logger.Error(err, "Failed to delete UpgradeDiagsCheck", "requestId", requestID)
		http.Error(w, "Internal error deleting upgrade diags check", http.StatusInternalServerError)
		return
	}
	s.Logger.Info("Upgrade diags check deleted via HTTP", "requestId", requestID, "crName", cr.Name)
	w.WriteHeader(http.StatusNoContent)
}

// cleanupExpiredChecks deletes readiness checks that have passed their TTL.
// This runs opportunistically when new requests come in to avoid resource buildup.
func (s *UpgradeDiagsHTTPServer) cleanupExpiredChecks(ctx context.Context) {
	list := &upgradev1alpha1.UpgradeDiagsCheckList{}
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
		deleteAfter := cr.Annotations["upgradediags.managed.openshift.io/delete-after"]
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
				s.Logger.Info("Deleted expired upgrade diags check", "crName", cr.Name, "expiry", deleteAfter)
			}
		}
	}
}

// checkActiveLimit enforces a maximum of 10 active readiness checks to prevent resource exhaustion.
// Returns an error with instructions if the limit is reached.
func (s *UpgradeDiagsHTTPServer) checkActiveLimit(ctx context.Context) error {
	const maxActiveChecks = 10
	list := &upgradev1alpha1.UpgradeDiagsCheckList{}
	if err := s.Client.List(ctx, list,
		client.InNamespace(s.Namespace),
		client.HasLabels{requestIDLabel},
	); err != nil {
		return fmt.Errorf("internal error checking active checks: %w", err)
	}
	if len(list.Items) >= maxActiveChecks {
		return fmt.Errorf("too many active upgrade diags checks (%d/%d). List existing checks with GET /api/v1/upgrade-diags and delete completed ones with DELETE /api/v1/upgrade-diags/{id} before creating new checks", len(list.Items), maxActiveChecks)
	}
	return nil
}

// authorizeToken performs TokenReview (authn) then SubjectAccessReview (authz).
// verb should be "create", "get", "list", or "delete".
func (s *UpgradeDiagsHTTPServer) authorizeToken(ctx context.Context, token, verb string) error {
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
				Resource:  "upgradediagschecks",
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

func (s *UpgradeDiagsHTTPServer) findCR(ctx context.Context, requestID string) (*upgradev1alpha1.UpgradeDiagsCheck, error) {
	list := &upgradev1alpha1.UpgradeDiagsCheckList{}
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

// crNamePrefix returns a sanitized GenerateName value.
// If raw is a valid non-empty caller-supplied prefix it is used as-is.
// Otherwise a descriptive prefix is derived from version and platform:
//
//	version="4.22.22", platform="OSD" → "urc-4-22-22-osd-"
func crNamePrefix(raw, version, platform string) string {
	sanitize := func(s string) string {
		s = strings.ToLower(s)
		var b strings.Builder
		for _, r := range s {
			switch {
			case r == '.' || r == '_':
				b.WriteRune('-')
			case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
				b.WriteRune(r)
			}
		}
		return strings.Trim(b.String(), "-")
	}
	if p := sanitize(raw); p != "" {
		return p + "-"
	}
	ver := sanitize(version)
	plat := sanitize(platform)
	switch {
	case ver != "" && plat != "":
		return fmt.Sprintf("udc-%s-%s-", ver, plat)
	case ver != "":
		return fmt.Sprintf("udc-%s-", ver)
	default:
		return "udc-"
	}
}

func (s *UpgradeDiagsHTTPServer) buildInternalCR(requestID string, req CheckRequest) *upgradev1alpha1.UpgradeDiagsCheck {
	annotations := map[string]string{
		// Auto-delete after 24 hours to prevent resource buildup
		"upgradediags.managed.openshift.io/delete-after": time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}
	if req.Description != "" {
		annotations["upgradediags.managed.openshift.io/description"] = req.Description
	}
	return &upgradev1alpha1.UpgradeDiagsCheck{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: crNamePrefix(req.NamePrefix, req.TargetVersion, req.Platform),
			Namespace:    s.Namespace,
			Labels:       map[string]string{requestIDLabel: requestID},
			Annotations:  annotations,
		},
		Spec: upgradev1alpha1.UpgradeDiagsCheckSpec{
			UpgradeConfigSpec: upgradev1alpha1.UpgradeConfigSpec{
				Type:                 upgradev1alpha1.UpgradeType(req.Platform),
				Desired:              upgradev1alpha1.Update{Version: req.TargetVersion, Channel: req.TargetChannel},
				UpgradeAt:            time.Now().Format(time.RFC3339),
				PDBForceDrainTimeout: 60,
			},
			DefaultCheckTimeoutSeconds: req.DefaultCheckTimeoutSeconds,
			OverallTimeoutSeconds:      req.OverallTimeoutSeconds,
			Checks:                     req.ChecksToRun,
		},
	}
}

func buildResponse(requestID string, cr *upgradev1alpha1.UpgradeDiagsCheck) CheckResponse {
	status := "pending"
	if cr.Status.StartedAt != nil {
		status = "running"
	}
	if cr.Status.CompletedAt != nil {
		status = "completed"
	}

	resp := CheckResponse{
		RequestID:        requestID,
		Status:           status,
		ClusterInfraName: cr.Status.ClusterInfraName,
		APIServerURL:     cr.Status.APIServerURL,
		StartedAt:        cr.Status.StartedAt,
		CompletedAt:      cr.Status.CompletedAt,
		TargetVersion:    cr.Spec.Desired.Version,
		TargetChannel:    cr.Spec.Desired.Channel,
		Platform:         string(cr.Spec.Type),
		ChecksTotal:      len(cr.Status.Checks),
	}

	anyBad := false
	anyWarning := false
	for _, c := range cr.Status.Checks {
		r := CheckResult{Name: c.Name, Owner: string(c.Owner), Action: c.Action}
		switch c.Execution.State {
		case upgradev1alpha1.CheckCompleted:
			resp.ChecksCompleted++
			r.Status = strings.ToLower(string(c.Result))
			r.Message = c.Execution.Summary
			r.Details = c.Execution.Findings
			switch c.Result {
			case upgradev1alpha1.CheckFailed:
				resp.ChecksFailed++
				anyBad = true
			case upgradev1alpha1.CheckResultWarning:
				resp.ChecksWarned++
				anyWarning = true
			default:
				resp.ChecksPassed++
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
		switch {
		case anyBad:
			resp.OverallStatus = "failed"
			resp.Message = fmt.Sprintf("Upgrade not recommended — %d check(s) failed, %d errored",
				resp.ChecksFailed, resp.ChecksErrored)
		case anyWarning:
			resp.OverallStatus = "warning"
			resp.Message = fmt.Sprintf("Upgrade may proceed — %d check(s) returned warnings, review findings before upgrading",
				resp.ChecksWarned)
		default:
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
	if req.DefaultCheckTimeoutSeconds < 0 {
		return fmt.Errorf("defaultCheckTimeoutSeconds must be >= 0, got %d", req.DefaultCheckTimeoutSeconds)
	}
	if req.OverallTimeoutSeconds < 0 {
		return fmt.Errorf("overallTimeoutSeconds must be >= 0, got %d", req.OverallTimeoutSeconds)
	}
	if len(req.ChecksToRun) > 0 {
		valid := make(map[string]struct{}, len(registeredChecks))
		for _, c := range registeredChecks {
			valid[c.Name()] = struct{}{}
		}
		var unknown []string
		for _, name := range req.ChecksToRun {
			if _, ok := valid[name]; !ok {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			known := make([]string, 0, len(registeredChecks))
			for _, c := range registeredChecks {
				known = append(known, c.Name())
			}
			return fmt.Errorf("unknown check name(s) in checksToRun: %v — valid names: %v", unknown, known)
		}
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

// routeAvailableVersions dispatches to handleAvailableVersions (GET only).
func (s *UpgradeDiagsHTTPServer) routeAvailableVersions(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			s.Logger.Error(fmt.Errorf("%v", p), "panic in available versions handler",
				"method", r.Method, "path", r.URL.Path)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()

	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "Authorization: Bearer <token> required", http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodGet {
		s.handleAvailableVersions(w, r, token)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// routeVersion handles GET /api/v1/version — no auth required, used by the console plugin to display build info.
func (s *UpgradeDiagsHTTPServer) routeVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := struct {
		Version   string `json:"version"`
		GitCommit string `json:"gitCommit"`
	}{
		Version:   version.Version,
		GitCommit: version.GitCommit,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
