package upgradediags

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ---- Response types --------------------------------------------------------

// UpgradeStatusSnapshot is the top-level response for GET /api/v1/upgrade-status.
// It is a live read — no CR is created; every field reflects cluster state at
// the moment the request is received.
type UpgradeStatusSnapshot struct {
	// Phase is the high-level upgrade lifecycle phase.
	// One of: not-started, pre-checks, control-plane, operators,
	//         worker-upgrade, complete, stalled, failed, unknown.
	Phase          string `json:"phase"`
	TargetVersion  string `json:"targetVersion,omitempty"`
	TargetChannel  string `json:"targetChannel,omitempty"`
	CurrentVersion string `json:"currentVersion,omitempty"`
	// CVOMessage is the human-readable message from the CVO Progressing condition.
	CVOMessage     string              `json:"cvoMessage,omitempty"`
	StartedAt      *metav1.Time        `json:"startedAt,omitempty"`
	ElapsedSeconds int64               `json:"elapsedSeconds,omitempty"`
	ControlPlane   ControlPlaneStatus  `json:"controlPlane"`
	Operators      OperatorsStatus     `json:"clusterOperators"`
	MachinePools   []MachinePoolStatus `json:"machinePools"`
	// Blockers lists conditions actively preventing upgrade progress.
	// Empty when the upgrade is proceeding normally or is not started.
	Blockers []UpgradeBlocker `json:"blockers,omitempty"`
	// MachineHealthChecks is the list of MachineHealthCheck objects from
	// openshift-machine-api, showing remediation status at upgrade time.
	MachineHealthChecks []MachineHealthCheckStatus `json:"machineHealthChecks,omitempty"`
}

type ControlPlaneStatus struct {
	// Phase: not-started, upgrading, complete, failed.
	Phase          string       `json:"phase"`
	StartedAt      *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt    *metav1.Time `json:"completedAt,omitempty"`
	ElapsedSeconds int64        `json:"elapsedSeconds,omitempty"`
	Message        string       `json:"message,omitempty"`
}

type OperatorsStatus struct {
	// Phase: not-started, progressing, complete, degraded.
	Phase          string           `json:"phase"`
	Total          int              `json:"total"`
	Available      int              `json:"available"`
	Progressing    int              `json:"progressing"`
	Degraded       int              `json:"degraded"`
	StartedAt      *metav1.Time     `json:"startedAt,omitempty"`
	CompletedAt    *metav1.Time     `json:"completedAt,omitempty"`
	ElapsedSeconds int64            `json:"elapsedSeconds,omitempty"`
	Operators      []OperatorStatus `json:"operators"`
}

type OperatorCondition struct {
	Type    string       `json:"type"`
	Status  string       `json:"status"`
	Reason  string       `json:"reason,omitempty"`
	Message string       `json:"message,omitempty"`
	Since   *metav1.Time `json:"since,omitempty"`
}

type OperatorStatus struct {
	Name        string `json:"name"`
	// Phase: available, progressing, degraded.
	Phase       string `json:"phase"`
	Version     string `json:"version,omitempty"`
	Available   bool   `json:"available"`
	Progressing bool   `json:"progressing"`
	Degraded    bool   `json:"degraded"`
	// Message is the most relevant condition message (Progressing or Degraded).
	Message               string               `json:"message,omitempty"`
	ProgressingSince      *metav1.Time         `json:"progressingSince,omitempty"`
	ElapsedInPhaseSeconds int64                `json:"elapsedInPhaseSeconds,omitempty"`
	Conditions            []OperatorCondition  `json:"conditions,omitempty"`
}

type MachinePoolStatus struct {
	Name string `json:"name"`
	// Phase: not-started, updating, updated, degraded.
	Phase          string       `json:"phase"`
	Total          int          `json:"total"`
	Updated        int          `json:"updated"`
	Ready          int          `json:"ready"`
	Unavailable    int          `json:"unavailable"`
	Degraded       int          `json:"degraded"`
	StartedAt      *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt    *metav1.Time `json:"completedAt,omitempty"`
	ElapsedSeconds int64        `json:"elapsedSeconds,omitempty"`
	Nodes          []NodeStatus `json:"nodes,omitempty"`
}

// DrainBlocker describes a PDB that is actively preventing a node drain.
type DrainBlocker struct {
	// PodName is the pod on the draining node that the PDB protects.
	PodName      string `json:"podName"`
	PodNamespace string `json:"podNamespace"`
	PDBName      string `json:"pdbName"`
	// Owner is the controller owning the pod, e.g. "Deployment/my-app" or "StatefulSet/my-db".
	Owner              string `json:"owner,omitempty"`
	DisruptionsAllowed int32  `json:"disruptionsAllowed"`
	DesiredHealthy     int32  `json:"desiredHealthy"`
	CurrentHealthy     int32  `json:"currentHealthy"`
	// Violation summarises why the PDB blocks drains, e.g. "MaxUnavailable=0".
	Violation string `json:"violation,omitempty"`
}

// ProviderError is an infrastructure-layer error surfaced from Machine objects
// or cloud-provider node events (e.g. IAM, capacity, quota, network errors).
type ProviderError struct {
	// Source is where the error was found: "Machine" or "NodeEvent".
	Source  string       `json:"source"`
	Reason  string       `json:"reason,omitempty"`
	Message string       `json:"message"`
	Since   *metav1.Time `json:"since,omitempty"`
}

type NodeCondition struct {
	Type    string       `json:"type"`
	Status  string       `json:"status"`
	Reason  string       `json:"reason,omitempty"`
	Message string       `json:"message,omitempty"`
	Since   *metav1.Time `json:"since,omitempty"`
}

type NodeStatus struct {
	Name string `json:"name"`
	// Phase: done, working, rebooting, degraded, draining, unknown.
	Phase         string       `json:"phase"`
	CurrentConfig string       `json:"currentConfig,omitempty"`
	DesiredConfig string       `json:"desiredConfig,omitempty"`
	// Roles lists node-role.kubernetes.io/* labels (e.g. master, infra, worker).
	Roles []string `json:"roles,omitempty"`
	// PhaseSince is when the node entered its current phase (from MachineConfig state annotation change).
	PhaseSince     *metav1.Time  `json:"phaseSince,omitempty"`
	// ElapsedSeconds is seconds since the NodeReady condition last transitioned.
	// For upgrading nodes this approximates time in the current upgrade phase.
	ElapsedSeconds int64         `json:"elapsedSeconds,omitempty"`
	// AgeSeconds is seconds since the node was created (node.CreationTimestamp).
	AgeSeconds     int64         `json:"ageSeconds,omitempty"`
	// DrainBlockers lists PDBs actively preventing this node's drain.
	// Only populated when phase is "draining" or "working".
	DrainBlockers  []DrainBlocker  `json:"drainBlockers,omitempty"`
	// ProviderErrors surfaces AWS/cloud-layer errors from Machine objects
	// (IAM failures, capacity issues, instance state errors, etc.).
	ProviderErrors []ProviderError `json:"providerErrors,omitempty"`
	// Conditions is the full list of Kubernetes node conditions for drill-down.
	Conditions     []NodeCondition `json:"conditions,omitempty"`
}

type UpgradeBlocker struct {
	// Type is a machine-readable blocker category:
	//   operator-degraded, operator-progressing-stalled,
	//   node-stalled, mcp-degraded, cvo-degraded.
	Type           string       `json:"type"`
	Component      string       `json:"component"`
	Message        string       `json:"message"`
	Since          *metav1.Time `json:"since,omitempty"`
	ElapsedSeconds int64        `json:"elapsedSeconds,omitempty"`
}

// MachineHealthCheckStatus summarises a single MachineHealthCheck object.
type MachineHealthCheckStatus struct {
	Name             string `json:"name"`
	// MaxUnhealthy is the configured threshold as a string ("2" or "100%").
	MaxUnhealthy     string `json:"maxUnhealthy"`
	ExpectedMachines int    `json:"expectedMachines"`
	CurrentHealthy   int    `json:"currentHealthy"`
	// RemediationsAllowed is 0 when the controller has paused remediation because
	// the number of unhealthy machines would exceed MaxUnhealthy.
	RemediationsAllowed int  `json:"remediationsAllowed"`
	// Remediating is true when currentHealthy < expectedMachines.
	Remediating         bool `json:"remediating"`
}

// UpgradeHistoryEntry is one entry from ClusterVersion's upgrade history.
type UpgradeHistoryEntry struct {
	Version     string       `json:"version"`
	// FromVersion is the version this cluster was on before upgrading to Version.
	// Derived from the next-older history entry.
	FromVersion string       `json:"fromVersion,omitempty"`
	State       string       `json:"state"`
	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// ElapsedSeconds is the duration of the upgrade (completedAt - startedAt).
	ElapsedSeconds int64  `json:"elapsedSeconds,omitempty"`
	Message        string `json:"message,omitempty"`
}

// ---- HTTP routing ----------------------------------------------------------

func (s *UpgradeDiagsHTTPServer) routeStatus(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			s.Logger.Error(fmt.Errorf("%v", p), "panic in upgrade status handler")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}()
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := extractBearerToken(r)
	if token == "" {
		http.Error(w, "Authorization: Bearer <token> required", http.StatusUnauthorized)
		return
	}
	if err := s.authorizeToken(r.Context(), token, "get"); err != nil {
		http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if strings.HasSuffix(path, "/history") {
		s.handleStatusHistory(w, r)
	} else {
		s.handleStatus(w, r)
	}
}

func (s *UpgradeDiagsHTTPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildStatusSnapshot(r.Context())
	if err != nil {
		s.Logger.Error(err, "Failed to build upgrade status snapshot")
		http.Error(w, "Internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *UpgradeDiagsHTTPServer) handleStatusHistory(w http.ResponseWriter, r *http.Request) {
	cv := &configv1.ClusterVersion{}
	if err := s.Client.Get(r.Context(), types.NamespacedName{Name: "version"}, cv); err != nil {
		http.Error(w, "Failed to read ClusterVersion: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// History is ordered most-recent-first. The "from version" of entry[i] is entry[i+1].version.
	entries := make([]UpgradeHistoryEntry, 0, len(cv.Status.History))
	for i, h := range cv.Status.History {
		e := UpgradeHistoryEntry{
			Version: h.Version,
			State:   string(h.State),
			Message: h.AcceptedRisks,
		}
		if i+1 < len(cv.Status.History) {
			e.FromVersion = cv.Status.History[i+1].Version
		}
		if !h.StartedTime.IsZero() {
			t := metav1.NewTime(h.StartedTime.Time)
			e.StartedAt = &t
		}
		if h.CompletionTime != nil && !h.CompletionTime.IsZero() {
			t := metav1.NewTime(h.CompletionTime.Time)
			e.CompletedAt = &t
			if !h.StartedTime.IsZero() {
				e.ElapsedSeconds = int64(h.CompletionTime.Sub(h.StartedTime.Time).Seconds())
			}
		}
		entries = append(entries, e)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"currentVersion": currentVersion(cv),
		"currentChannel": cv.Spec.Channel,
		"history":        entries,
		"total":          len(entries),
	})
}

// ---- Snapshot builder ------------------------------------------------------

func (s *UpgradeDiagsHTTPServer) buildStatusSnapshot(ctx context.Context) (*UpgradeStatusSnapshot, error) {
	now := time.Now()

	cv := &configv1.ClusterVersion{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: "version"}, cv); err != nil {
		return nil, fmt.Errorf("reading ClusterVersion: %w", err)
	}

	// ClusterOperators — errors are soft: we return partial data.
	coList := &configv1.ClusterOperatorList{}
	_ = s.Client.List(ctx, coList)

	// MachineConfigPools via unstructured — optional; skip on error.
	mcps, _ := s.listMCPs(ctx)

	// Nodes for per-node detail — optional.
	nodeList := &corev1.NodeList{}
	_ = s.Client.List(ctx, nodeList)

	// Pods (all namespaces) for drain-blocker mapping — optional.
	podList := &corev1.PodList{}
	_ = s.Client.List(ctx, podList)

	// PodDisruptionBudgets (all namespaces) — optional.
	pdbList := &policyv1.PodDisruptionBudgetList{}
	_ = s.Client.List(ctx, pdbList)

	// Machine objects (OpenShift Machine API) for cloud-provider errors — optional.
	machines, _ := s.listMachines(ctx)

	// MachineHealthChecks — optional; skip on error.
	mhcs, _ := s.listMHCs(ctx)

	snap := &UpgradeStatusSnapshot{}
	snap.CurrentVersion = currentVersion(cv)
	snap.TargetVersion = targetVersion(cv)
	snap.TargetChannel = cv.Spec.Channel

	// Upgrade start time: find the history entry for the target version.
	if snap.TargetVersion != "" {
		for _, h := range cv.Status.History {
			if h.Version == snap.TargetVersion {
				t := metav1.NewTime(h.StartedTime.Time)
				snap.StartedAt = &t
				break
			}
		}
	}
	if snap.StartedAt != nil && !snap.StartedAt.IsZero() {
		snap.ElapsedSeconds = int64(now.Sub(snap.StartedAt.Time).Seconds())
	}

	// CVOMessage from Progressing condition.
	for _, c := range cv.Status.Conditions {
		if c.Type == configv1.OperatorProgressing && c.Status == configv1.ConditionTrue {
			snap.CVOMessage = c.Message
			break
		}
	}

	// Build sub-statuses.
	snap.ControlPlane = buildControlPlaneStatus(cv, now)
	snap.Operators = buildOperatorsStatus(coList, now)
	snap.MachinePools = buildMachinePoolStatuses(mcps, nodeList, podList, pdbList, machines, now)

	// Determine top-level phase.
	snap.Phase = detectUpgradePhase(cv, snap.ControlPlane, snap.Operators, snap.MachinePools)

	// Collect blockers.
	snap.Blockers = collectBlockers(cv, snap.Operators, snap.MachinePools, now)

	// MachineHealthChecks.
	snap.MachineHealthChecks = buildMHCStatuses(mhcs)

	return snap, nil
}

// ---- ClusterVersion helpers ------------------------------------------------

func currentVersion(cv *configv1.ClusterVersion) string {
	for _, h := range cv.Status.History {
		if h.State == configv1.CompletedUpdate {
			return h.Version
		}
	}
	return cv.Status.Desired.Version
}

func targetVersion(cv *configv1.ClusterVersion) string {
	if cv.Spec.DesiredUpdate != nil && cv.Spec.DesiredUpdate.Version != "" {
		return cv.Spec.DesiredUpdate.Version
	}
	return ""
}

func cvoCondition(cv *configv1.ClusterVersion, t configv1.ClusterStatusConditionType) (status bool, msg string, since metav1.Time) {
	for _, c := range cv.Status.Conditions {
		if c.Type == t {
			return c.Status == configv1.ConditionTrue, c.Message, c.LastTransitionTime
		}
	}
	return false, "", metav1.Time{}
}

// ---- Control plane status --------------------------------------------------

func buildControlPlaneStatus(cv *configv1.ClusterVersion, now time.Time) ControlPlaneStatus {
	progressing, progressMsg, progressSince := cvoCondition(cv, configv1.OperatorProgressing)
	degraded, degradeMsg, _ := cvoCondition(cv, configv1.OperatorDegraded)
	available, _, _ := cvoCondition(cv, configv1.OperatorAvailable)

	target := targetVersion(cv)
	current := currentVersion(cv)

	var cp ControlPlaneStatus

	switch {
	case degraded && !progressing:
		cp.Phase = "failed"
		cp.Message = degradeMsg
	case progressing:
		cp.Phase = "upgrading"
		cp.Message = progressMsg
		if !progressSince.IsZero() {
			cp.StartedAt = &progressSince
			cp.ElapsedSeconds = int64(now.Sub(progressSince.Time).Seconds())
		}
	case available && target != "" && target == current:
		cp.Phase = "complete"
		// Find completion time from history.
		for _, h := range cv.Status.History {
			if h.Version == target && h.State == configv1.CompletedUpdate {
				if !h.StartedTime.IsZero() {
					t := metav1.NewTime(h.StartedTime.Time)
					cp.StartedAt = &t
				}
				if h.CompletionTime != nil && !h.CompletionTime.IsZero() {
					cp.CompletedAt = h.CompletionTime
					if cp.StartedAt != nil {
						cp.ElapsedSeconds = int64(cp.CompletedAt.Time.Sub(cp.StartedAt.Time).Seconds())
					}
				}
				break
			}
		}
	default:
		cp.Phase = "not-started"
	}

	return cp
}

// ---- ClusterOperator status ------------------------------------------------

func buildOperatorsStatus(coList *configv1.ClusterOperatorList, now time.Time) OperatorsStatus {
	os := OperatorsStatus{
		Total:     len(coList.Items),
		Operators: make([]OperatorStatus, 0, len(coList.Items)),
	}

	var earliestProgressing, latestProgressing metav1.Time

	for _, co := range coList.Items {
		op := OperatorStatus{Name: co.Name}

		for _, c := range co.Status.Conditions {
			switch c.Type {
			case configv1.OperatorAvailable:
				op.Available = c.Status == configv1.ConditionTrue
			case configv1.OperatorProgressing:
				op.Progressing = c.Status == configv1.ConditionTrue
				if op.Progressing {
					op.Message = c.Message
					op.ProgressingSince = &c.LastTransitionTime
					op.ElapsedInPhaseSeconds = int64(now.Sub(c.LastTransitionTime.Time).Seconds())
					if earliestProgressing.IsZero() || c.LastTransitionTime.Before(&earliestProgressing) {
						earliestProgressing = c.LastTransitionTime
					}
					if c.LastTransitionTime.After(latestProgressing.Time) {
						latestProgressing = c.LastTransitionTime
					}
				}
			case configv1.OperatorDegraded:
				op.Degraded = c.Status == configv1.ConditionTrue
				if op.Degraded && op.Message == "" {
					op.Message = c.Message
				}
			}
		}

		// Operator version: take the first entry from Status.Versions.
		if len(co.Status.Versions) > 0 {
			op.Version = co.Status.Versions[0].Version
		}

		// Full conditions list for drill-down.
		for _, c := range co.Status.Conditions {
			cond := OperatorCondition{
				Type:    string(c.Type),
				Status:  string(c.Status),
				Reason:  c.Reason,
				Message: c.Message,
			}
			if !c.LastTransitionTime.IsZero() {
				t := c.LastTransitionTime
				cond.Since = &t
			}
			op.Conditions = append(op.Conditions, cond)
		}

		switch {
		case op.Degraded:
			op.Phase = "degraded"
			os.Degraded++
		case op.Progressing:
			op.Phase = "progressing"
			os.Progressing++
		case op.Available:
			op.Phase = "available"
			os.Available++
		default:
			op.Phase = "unknown"
		}

		os.Operators = append(os.Operators, op)
	}

	switch {
	case os.Degraded > 0:
		os.Phase = "degraded"
	case os.Progressing > 0:
		os.Phase = "progressing"
		if !earliestProgressing.IsZero() {
			os.StartedAt = &earliestProgressing
			os.ElapsedSeconds = int64(now.Sub(earliestProgressing.Time).Seconds())
		}
	case os.Total > 0 && os.Available == os.Total:
		os.Phase = "complete"
	default:
		os.Phase = "not-started"
	}

	return os
}

// ---- MachineConfigPool status ----------------------------------------------

var mcpGVK = schema.GroupVersionKind{
	Group:   "machineconfiguration.openshift.io",
	Version: "v1",
	Kind:    "MachineConfigPoolList",
}

func (s *UpgradeDiagsHTTPServer) listMCPs(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mcpGVK)
	if err := s.Client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func buildMachinePoolStatuses(mcps []unstructured.Unstructured, nodeList *corev1.NodeList, podList *corev1.PodList, pdbList *policyv1.PodDisruptionBudgetList, machines []unstructured.Unstructured, now time.Time) []MachinePoolStatus {
	// Index nodes by pool membership (role labels).
	nodesByPool := make(map[string][]corev1.Node)
	for _, node := range nodeList.Items {
		for label := range node.Labels {
			if strings.HasPrefix(label, "node-role.kubernetes.io/") {
				role := strings.TrimPrefix(label, "node-role.kubernetes.io/")
				nodesByPool[role] = append(nodesByPool[role], node)
			}
		}
	}

	// Index pods by node name for drain-blocker lookup.
	podsByNode := make(map[string][]corev1.Pod, len(nodeList.Items))
	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			podsByNode[pod.Spec.NodeName] = append(podsByNode[pod.Spec.NodeName], pod)
		}
	}

	// Index machine provider errors by node name.
	machineErrsByNode := buildMachineErrorIndex(machines)

	pools := make([]MachinePoolStatus, 0, len(mcps))
	for _, mcp := range mcps {
		name, _, _ := unstructured.NestedString(mcp.Object, "metadata", "name")
		status, _, _ := unstructured.NestedMap(mcp.Object, "status")

		ps := MachinePoolStatus{Name: name}
		ps.Total = intField(status, "machineCount")
		ps.Updated = intField(status, "updatedMachineCount")
		ps.Ready = intField(status, "readyMachineCount")
		ps.Unavailable = intField(status, "unavailableMachineCount")
		ps.Degraded = intField(status, "degradedMachineCount")

		// Timestamps from MCP conditions.
		if conds, ok := status["conditions"].([]interface{}); ok {
			for _, ci := range conds {
				c, ok := ci.(map[string]interface{})
				if !ok {
					continue
				}
				ctype, _ := c["type"].(string)
				cstatus, _ := c["status"].(string)
				transitionTime, _ := c["lastTransitionTime"].(string)
				t, _ := time.Parse(time.RFC3339, transitionTime)
				mt := metav1.NewTime(t)

				switch ctype {
				case "Updating":
					if cstatus == "True" && !t.IsZero() {
						ps.StartedAt = &mt
						ps.ElapsedSeconds = int64(now.Sub(t).Seconds())
					}
				case "Updated":
					if cstatus == "True" && !t.IsZero() {
						ps.CompletedAt = &mt
						if ps.StartedAt != nil {
							ps.ElapsedSeconds = int64(ps.CompletedAt.Time.Sub(ps.StartedAt.Time).Seconds())
						}
					}
				}
			}
		}

		switch {
		case ps.Degraded > 0:
			ps.Phase = "degraded"
		case ps.Updated == ps.Total && ps.Total > 0:
			ps.Phase = "updated"
		case ps.Unavailable > 0 || (ps.Updated < ps.Total && ps.Total > 0):
			ps.Phase = "updating"
		default:
			ps.Phase = "not-started"
		}

		// Per-node details from the node list for this pool's role.
		for _, node := range nodesByPool[name] {
			ps.Nodes = append(ps.Nodes, buildNodeStatus(node, podsByNode[node.Name], pdbList.Items, machineErrsByNode[node.Name], now))
		}

		pools = append(pools, ps)
	}
	return pools
}

func buildNodeStatus(node corev1.Node, nodePods []corev1.Pod, pdbs []policyv1.PodDisruptionBudget, providerErrors []ProviderError, now time.Time) NodeStatus {
	ann := node.Annotations
	if ann == nil {
		ann = map[string]string{}
	}
	// Collect node roles from node-role.kubernetes.io/* labels.
	var roles []string
	for label := range node.Labels {
		if strings.HasPrefix(label, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(label, "node-role.kubernetes.io/")
			if role != "" {
				roles = append(roles, role)
			}
		}
	}
	sort.Strings(roles)

	ns := NodeStatus{
		Name:           node.Name,
		CurrentConfig:  ann["machineconfiguration.openshift.io/currentConfig"],
		DesiredConfig:  ann["machineconfiguration.openshift.io/desiredConfig"],
		Roles:          roles,
		AgeSeconds:     int64(now.Sub(node.CreationTimestamp.Time).Seconds()),
		ProviderErrors: providerErrors,
	}

	mcState := strings.ToLower(ann["machineconfiguration.openshift.io/state"])
	switch mcState {
	case "done":
		ns.Phase = "done"
	case "working":
		ns.Phase = "working"
	case "rebooting":
		ns.Phase = "rebooting"
	case "degraded":
		ns.Phase = "degraded"
	default:
		if node.Spec.Unschedulable {
			ns.Phase = "draining"
		} else {
			ns.Phase = "unknown"
		}
	}

	// Use node condition LastTransitionTime as a proxy for when phase started.
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			ns.PhaseSince = &cond.LastTransitionTime
			ns.ElapsedSeconds = int64(now.Sub(cond.LastTransitionTime.Time).Seconds())
			break
		}
	}

	// Find PDBs actively blocking drain for nodes that are draining or stuck working.
	if ns.Phase == "draining" || ns.Phase == "working" {
		ns.DrainBlockers = findDrainBlockers(node, nodePods, pdbs)
	}

	// Full K8s node conditions for drill-down.
	for _, cond := range node.Status.Conditions {
		nc := NodeCondition{
			Type:    string(cond.Type),
			Status:  string(cond.Status),
			Reason:  cond.Reason,
			Message: cond.Message,
		}
		if !cond.LastTransitionTime.IsZero() {
			t := cond.LastTransitionTime
			nc.Since = &t
		}
		ns.Conditions = append(ns.Conditions, nc)
	}

	return ns
}

// ---- Phase detection -------------------------------------------------------

func detectUpgradePhase(cv *configv1.ClusterVersion, cp ControlPlaneStatus, ops OperatorsStatus, pools []MachinePoolStatus) string {
	target := targetVersion(cv)
	current := currentVersion(cv)

	if target == "" {
		return "not-started"
	}

	progressing, _, _ := cvoCondition(cv, configv1.OperatorProgressing)
	degraded, _, _ := cvoCondition(cv, configv1.OperatorDegraded)

	// Upgrade complete: CVO says not progressing, available, and at target.
	if !progressing && !degraded && target == current {
		return "complete"
	}

	if degraded && !progressing {
		return "failed"
	}

	if !progressing && target != current {
		return "pre-checks"
	}

	// Progressing: determine which phase by what's still pending.
	masterDone := true
	workerDone := true
	for _, p := range pools {
		if p.Name == "master" && p.Phase != "updated" {
			masterDone = false
		}
		if p.Name != "master" && p.Phase != "updated" {
			workerDone = false
		}
	}

	switch {
	case !masterDone:
		return "control-plane"
	case ops.Phase == "progressing" || ops.Phase == "degraded":
		return "operators"
	case !workerDone:
		return "worker-upgrade"
	default:
		return "operators" // CVO is progressing but we can't tell more precisely
	}
}

// ---- Blocker detection -----------------------------------------------------

const (
	operatorStalledThreshold = 10 * time.Minute
	nodeStalledThreshold     = 30 * time.Minute
)

func collectBlockers(cv *configv1.ClusterVersion, ops OperatorsStatus, pools []MachinePoolStatus, now time.Time) []UpgradeBlocker {
	var blockers []UpgradeBlocker

	// CVO degraded.
	if degraded, msg, since := cvoCondition(cv, configv1.OperatorDegraded); degraded {
		elapsed := int64(now.Sub(since.Time).Seconds())
		blockers = append(blockers, UpgradeBlocker{
			Type:           "cvo-degraded",
			Component:      "cluster-version-operator",
			Message:        msg,
			Since:          &since,
			ElapsedSeconds: elapsed,
		})
	}

	// Degraded or long-stalled operators.
	for _, op := range ops.Operators {
		if op.Degraded {
			var since *metav1.Time
			var elapsed int64
			if op.ProgressingSince != nil {
				since = op.ProgressingSince
				elapsed = int64(now.Sub(op.ProgressingSince.Time).Seconds())
			}
			blockers = append(blockers, UpgradeBlocker{
				Type:           "operator-degraded",
				Component:      op.Name,
				Message:        op.Message,
				Since:          since,
				ElapsedSeconds: elapsed,
			})
		} else if op.Progressing && op.ProgressingSince != nil &&
			now.Sub(op.ProgressingSince.Time) > operatorStalledThreshold {
			elapsed := int64(now.Sub(op.ProgressingSince.Time).Seconds())
			blockers = append(blockers, UpgradeBlocker{
				Type:           "operator-progressing-stalled",
				Component:      op.Name,
				Message:        fmt.Sprintf("Progressing for %dm: %s", elapsed/60, op.Message),
				Since:          op.ProgressingSince,
				ElapsedSeconds: elapsed,
			})
		}
	}

	// Degraded MCPs, stalled nodes, and active drain blockers.
	for _, pool := range pools {
		if pool.Degraded > 0 {
			blockers = append(blockers, UpgradeBlocker{
				Type:      "mcp-degraded",
				Component: pool.Name,
				Message:   fmt.Sprintf("%d/%d machines degraded in pool %q", pool.Degraded, pool.Total, pool.Name),
				Since:     pool.StartedAt,
			})
		}
		for _, node := range pool.Nodes {
			if node.Phase == "working" && node.PhaseSince != nil &&
				now.Sub(node.PhaseSince.Time) > nodeStalledThreshold {
				elapsed := int64(now.Sub(node.PhaseSince.Time).Seconds())
				blockers = append(blockers, UpgradeBlocker{
					Type:           "node-stalled",
					Component:      node.Name,
					Message:        fmt.Sprintf("Node has been in %q phase for %dm", node.Phase, elapsed/60),
					Since:          node.PhaseSince,
					ElapsedSeconds: elapsed,
				})
			}
			// Surface PDB drain blockers at the top level for fast visibility.
			for _, db := range node.DrainBlockers {
				msg := fmt.Sprintf("PDB %s/%s (owner: %s) blocking drain — DisruptionsAllowed=%d, DesiredHealthy=%d, CurrentHealthy=%d",
					db.PodNamespace, db.PDBName, db.Owner,
					db.DisruptionsAllowed, db.DesiredHealthy, db.CurrentHealthy)
				if db.Violation != "" {
					msg += fmt.Sprintf(" [%s]", db.Violation)
				}
				blockers = append(blockers, UpgradeBlocker{
					Type:      "pdb-drain-blocker",
					Component: node.Name,
					Message:   msg,
					Since:     node.PhaseSince,
				})
			}
			// Surface machine/cloud errors at the top level.
			for _, pe := range node.ProviderErrors {
				blockers = append(blockers, UpgradeBlocker{
					Type:      "provider-error",
					Component: node.Name,
					Message:   pe.Message,
					Since:     pe.Since,
				})
			}
		}
	}

	return blockers
}

// ---- Drain blocker detection -----------------------------------------------

// findDrainBlockers finds PDBs whose DisruptionsAllowed == 0 that cover any
// running pod on this node, indicating the PDB is actively preventing drain.
// It reuses podMatchesPDBSelector from check_pdb.go.
func findDrainBlockers(node corev1.Node, nodePods []corev1.Pod, pdbs []policyv1.PodDisruptionBudget) []DrainBlocker {
	var blockers []DrainBlocker
	seenPDB := make(map[string]bool) // namespace/name — one entry per PDB

	for _, pod := range nodePods {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, pdb := range pdbs {
			if pdb.Namespace != pod.Namespace {
				continue
			}
			if pdb.Status.DisruptionsAllowed > 0 {
				continue // PDB is not currently blocking
			}
			if !podMatchesPDBSelector(pod, pdb) {
				continue
			}
			key := pdb.Namespace + "/" + pdb.Name
			if seenPDB[key] {
				continue
			}
			seenPDB[key] = true
			blockers = append(blockers, DrainBlocker{
				PodName:            pod.Name,
				PodNamespace:       pod.Namespace,
				PDBName:            pdb.Name,
				Owner:              podOwnerRef(pod),
				DisruptionsAllowed: pdb.Status.DisruptionsAllowed,
				DesiredHealthy:     pdb.Status.DesiredHealthy,
				CurrentHealthy:     pdb.Status.CurrentHealthy,
				Violation:          pdbViolation(pdb),
			})
		}
	}
	return blockers
}

func podOwnerRef(pod corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind + "/" + ref.Name
		}
	}
	return ""
}

// ---- MachineHealthCheck status ---------------------------------------------

var mhcListGVK = schema.GroupVersionKind{
	Group:   "machine.openshift.io",
	Version: "v1beta1",
	Kind:    "MachineHealthCheckList",
}

func (s *UpgradeDiagsHTTPServer) listMHCs(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mhcListGVK)
	if err := s.Client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func buildMHCStatuses(mhcs []unstructured.Unstructured) []MachineHealthCheckStatus {
	out := make([]MachineHealthCheckStatus, 0, len(mhcs))
	for _, mhc := range mhcs {
		name, _, _ := unstructured.NestedString(mhc.Object, "metadata", "name")
		maxUnhealthy, _, _ := unstructured.NestedString(mhc.Object, "spec", "maxUnhealthy")

		status, _, _ := unstructured.NestedMap(mhc.Object, "status")
		expected  := intField(status, "expectedMachines")
		healthy   := intField(status, "currentHealthy")
		allowed   := intField(status, "remediationsAllowed")

		out = append(out, MachineHealthCheckStatus{
			Name:                name,
			MaxUnhealthy:        maxUnhealthy,
			ExpectedMachines:    expected,
			CurrentHealthy:      healthy,
			RemediationsAllowed: allowed,
			Remediating:         expected > 0 && healthy < expected,
		})
	}
	return out
}

// ---- Machine provider error detection --------------------------------------

var machineListGVK = schema.GroupVersionKind{
	Group:   "machine.openshift.io",
	Version: "v1beta1",
	Kind:    "MachineList",
}

func (s *UpgradeDiagsHTTPServer) listMachines(ctx context.Context) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(machineListGVK)
	if err := s.Client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// buildMachineErrorIndex returns a map of nodeName → []ProviderError by
// reading Machine object conditions and providerStatus for AWS/cloud errors.
func buildMachineErrorIndex(machines []unstructured.Unstructured) map[string][]ProviderError {
	idx := make(map[string][]ProviderError)
	for _, m := range machines {
		nodeName, _, _ := unstructured.NestedString(m.Object, "status", "nodeRef", "name")
		if nodeName == "" {
			continue
		}

		var errs []ProviderError

		// Machine conditions: surface Failed/Terminating with True status.
		if conds, ok, _ := unstructured.NestedSlice(m.Object, "status", "conditions"); ok {
			for _, ci := range conds {
				c, ok := ci.(map[string]interface{})
				if !ok {
					continue
				}
				ctype, _   := c["type"].(string)
				cstatus, _ := c["status"].(string)
				message, _ := c["message"].(string)
				reason, _  := c["reason"].(string)
				tsStr, _   := c["lastTransitionTime"].(string)

				if cstatus != "True" || message == "" {
					continue
				}
				// Report explicitly bad conditions and also Ready=False.
				if ctype != "Failed" && ctype != "Terminating" && !(ctype == "Ready" && cstatus == "False") {
					continue
				}
				t, _ := time.Parse(time.RFC3339, tsStr)
				mt := metav1.NewTime(t)
				errs = append(errs, ProviderError{
					Source:  "Machine",
					Reason:  reason,
					Message: fmt.Sprintf("%s: %s", ctype, message),
					Since:   &mt,
				})
			}
		}

		// providerStatus.instanceState — flag non-running AWS instance states.
		if ps, ok, _ := unstructured.NestedMap(m.Object, "status", "providerStatus"); ok {
			if instanceState, ok := ps["instanceState"].(string); ok && instanceState != "" && instanceState != "running" {
				errs = append(errs, ProviderError{
					Source:  "Machine",
					Reason:  "AWSInstanceState",
					Message: fmt.Sprintf("AWS instance is in state %q (expected running)", instanceState),
				})
			}
		}

		// errorMessage / errorReason at status root — set when machine is terminal.
		if errMsg, ok, _ := unstructured.NestedString(m.Object, "status", "errorMessage"); ok && errMsg != "" {
			errReason, _, _ := unstructured.NestedString(m.Object, "status", "errorReason")
			errs = append(errs, ProviderError{
				Source:  "Machine",
				Reason:  errReason,
				Message: errMsg,
			})
		}

		if len(errs) > 0 {
			idx[nodeName] = errs
		}
	}
	return idx
}

// ---- Utilities -------------------------------------------------------------

func intField(m map[string]interface{}, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}
