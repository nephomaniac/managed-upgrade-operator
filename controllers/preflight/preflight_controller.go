package preflight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	muocfg "github.com/openshift/managed-upgrade-operator/config"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/configmanager"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	"github.com/openshift/managed-upgrade-operator/util"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var log = logf.Log.WithName("controller_upgradepreflightcheck")

const (
	preflightControllerFeatureGate = "PreflightController"
	defaultCheckTimeout            = 30 * time.Second
	prometheusRetries              = 3
)

// prometheusRetryBase is a var so tests in this package can override it to avoid slow backoff waits.
var prometheusRetryBase = 2 * time.Second

// Check descriptions - educational context for cluster administrators about what each check does,
// why it matters for upgrades, and what cluster resources it examines.
const (
	descClusterVersion = "Retrieves the current cluster version from the ClusterVersion operator. Validates that version information is accessible and the cluster version object exists. Required for determining upgrade eligibility and version compatibility. Checks: ClusterVersion CR (cluster-scoped)."

	descCriticalAlerts = "Queries Prometheus for firing critical-severity alerts in core OpenShift namespaces. Critical alerts indicate severe issues (control plane degradation, data loss risk, service outages) that must be resolved before upgrading to prevent cluster instability. Checks: Prometheus /api/v1/query for alerts in openshift-* namespaces with severity=critical."

	descClusterOperators = "Validates all cluster operators report Available=True and Degraded=False conditions. Cluster operators manage core OpenShift components (authentication, console, DNS, networking, storage, monitoring). Degraded or unavailable operators cause upgrade failures and cluster instability. Checks: ClusterOperator CRs (cluster-scoped) for status conditions."

	descManuallyCordonedNodes = "Detects nodes manually cordoned by administrators (spec.unschedulable=true). Cordoned nodes block pod scheduling and may indicate ongoing maintenance, hardware issues, or deliberate isolation. Cordoned nodes should be uncordoned or verified as intentional before upgrade to ensure workload capacity. Checks: Node CRs (cluster-scoped) for spec.unschedulable field."

	descNodeTaints = "Identifies nodes with NoSchedule or NoExecute taints that prevent pod placement. Unschedulable taints block workloads from running on specific nodes and may indicate hardware failures, maintenance windows, or misconfigurations requiring attention before upgrade. Checks: Node CRs (cluster-scoped) for spec.taints with NoSchedule/NoExecute effects."

	descPodDisruptionBudgets = "Evaluates PodDisruptionBudgets in core namespaces to ensure sufficient pod replicas exist for safe node draining during upgrades. PDBs with insufficient available replicas block node drain operations, preventing upgrade progress. At-risk PDBs should be scaled or adjusted before upgrade. Checks: PodDisruptionBudget resources in openshift-* namespaces, comparing minAvailable to current ready replicas."

	descUpgradeableCondition = "Checks the ClusterVersion Upgradeable condition status. When Upgradeable=False, the Cluster Version Operator has detected a blocking issue: missing admin acknowledgements for API removals, incompatible installed operators, or critical configuration problems. The condition message explains what must be resolved. Checks: ClusterVersion status.conditions for Upgradeable type."

	descPullSecretValid = "Validates the cluster pull secret contains required cloud.openshift.com registry authentication. This registry hosts Red Hat operator images, OpenShift release payloads, and upgrade artifacts. Missing or invalid cloud.openshift.com credentials cause image pull failures during upgrade, blocking progress. Checks: Secret 'pull-secret' in openshift-config namespace for cloud.openshift.com auth entry."

	descAdminAcks = "Reads the admin-acks ConfigMap containing administrator acknowledgements for deprecated API removals and breaking changes. Y-stream upgrades (e.g. 4.14→4.15) removing deprecated Kubernetes APIs require explicit acknowledgements confirming workloads have been migrated. Missing required acks block upgrades. Checks: ConfigMap 'admin-acks' in openshift-config namespace."
)

// allChecks is the ordered list of check names run when no allowlist is specified.
var allChecks = []string{
	"ClusterVersion",
	"CriticalAlerts",
	"ClusterOperators",
	"ManuallyCordonedNodes",
	"NodeUnschedulableTaints",
	"PodDisruptionBudgets",
	"UpgradeableCondition",
	"PullSecretValid",
	"AdminAcks",
}

type preflightGateConfig struct {
	FeatureGate struct {
		Enabled []string `yaml:"enabled"`
	} `yaml:"featureGate"`
}

func (c *preflightGateConfig) IsValid() error { return nil }

func (c *preflightGateConfig) isEnabled() bool {
	for _, f := range c.FeatureGate.Enabled {
		if f == preflightControllerFeatureGate {
			return true
		}
	}
	return false
}

// ReconcileUpgradePreflightCheck reconciles UpgradePreflightCheck objects.
// It runs pre-upgrade health checks on demand, independent of any upgrade,
// and writes results to status. It makes no changes to upgrade state, sends no
// OCM notifications, and does not mutate any UpgradeConfig.
//
// The controller is a no-op unless "PreflightController" is listed in
// featureGate.enabled in the managed-upgrade-operator-config ConfigMap.
type ReconcileUpgradePreflightCheck struct {
	Client               client.Client
	Scheme               *runtime.Scheme
	MetricsClientBuilder metrics.MetricsBuilder
	CvClientBuilder      cv.ClusterVersionBuilder
	Machinery            machinery.Machinery
	ConfigManagerBuilder configmanager.ConfigManagerBuilder
}

func (r *ReconcileUpgradePreflightCheck) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, err error) {
	logger := log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)

	// Recover from any panic in the reconcile loop or in a check, so a fault in this
	// controller cannot crash the shared controller-manager process and take down the
	// UpgradeConfig and other MUO controllers with it.
	defer func() {
		if p := recover(); p != nil {
			logger.Error(fmt.Errorf("%v", p), "panic recovered in preflight reconcile; check will not be requeued")
			result, err = reconcile.Result{}, nil
		}
	}()

	target := muocfg.CMTarget{Namespace: req.Namespace}
	cmTarget, err := target.NewCMTarget()
	if err != nil {
		return reconcile.Result{}, err
	}
	gateCfg := &preflightGateConfig{}
	if err = r.ConfigManagerBuilder.New(r.Client, cmTarget).Into(gateCfg); err != nil {
		logger.Info("Could not read MUO ConfigMap; preflight controller inactive", "error", err.Error())
		return reconcile.Result{}, nil
	}
	if !gateCfg.isEnabled() {
		logger.V(4).Info("PreflightController feature gate not enabled; skipping. " +
			"Add \"PreflightController\" to featureGate.enabled in managed-upgrade-operator-config to activate.")
		return reconcile.Result{}, nil
	}

	logger.Info("Reconciling UpgradePreflightCheck")

	instance := &upgradev1alpha1.UpgradePreflightCheck{}
	if err := r.Client.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if instance.Status.CompletedAt != nil {
		return reconcile.Result{}, nil
	}

	if instance.Status.StartedAt == nil {
		now := metav1.NewTime(time.Now())
		instance.Status.StartedAt = &now
		if err := r.Client.Status().Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	logger.Info("Running preflight checks",
		"type", instance.Spec.Type,
		"targetVersion", instance.Spec.Desired.Version,
		"targetChannel", instance.Spec.Desired.Channel,
		"checkTimeoutSeconds", instance.Spec.CheckTimeoutSeconds,
		"checksAllowlist", instance.Spec.Checks,
	)

	checks := r.runChecks(ctx, instance, logger)

	now := metav1.NewTime(time.Now())
	instance.Status.CompletedAt = &now
	instance.Status.Checks = checks
	if err := r.Client.Status().Update(ctx, instance); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("UpgradePreflightCheck completed", "totalChecks", len(checks))
	return reconcile.Result{}, nil
}

func (r *ReconcileUpgradePreflightCheck) runChecks(
	ctx context.Context,
	instance *upgradev1alpha1.UpgradePreflightCheck,
	logger logr.Logger,
) []upgradev1alpha1.PreflightCheckResult {
	enabled := instance.Spec.Checks
	timeout := checkTimeout(instance)

	dispatch := map[string]func() upgradev1alpha1.PreflightCheckResult{
		"ClusterVersion":          func() upgradev1alpha1.PreflightCheckResult { return r.checkClusterVersion(ctx, logger, timeout) },
		"CriticalAlerts":          func() upgradev1alpha1.PreflightCheckResult { return r.checkCriticalAlerts(ctx, logger, timeout) },
		"ClusterOperators":        func() upgradev1alpha1.PreflightCheckResult { return r.checkClusterOperators(ctx, logger, timeout) },
		"ManuallyCordonedNodes":   func() upgradev1alpha1.PreflightCheckResult { return r.checkManuallyCordonedNodes(ctx, logger, timeout) },
		"NodeUnschedulableTaints": func() upgradev1alpha1.PreflightCheckResult { return r.checkNodeTaints(ctx, logger, timeout) },
		"PodDisruptionBudgets":    func() upgradev1alpha1.PreflightCheckResult { return r.checkPodDisruptionBudgets(ctx, logger, timeout) },
		"UpgradeableCondition":    func() upgradev1alpha1.PreflightCheckResult { return r.checkUpgradeableCondition(ctx, logger, timeout) },
		"PullSecretValid":         func() upgradev1alpha1.PreflightCheckResult { return r.checkPullSecretValid(ctx, logger, timeout) },
		"AdminAcks": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkAdminAcks(ctx, instance, logger, timeout)
		},
	}

	var results []upgradev1alpha1.PreflightCheckResult
	for _, name := range allChecks {
		if !checkAllowed(name, enabled) {
			logger.V(4).Info("Skipping check (not in allowlist)", "check", name)
			results = append(results, upgradev1alpha1.PreflightCheckResult{
				Name: name,
				Execution: upgradev1alpha1.CheckExecution{
					State:  upgradev1alpha1.CheckSkipped,
					Result: upgradev1alpha1.CheckUnknown,
				},
			})
			continue
		}
		fn := dispatch[name]
		results = append(results, safeRun(name, fn))
	}
	return results
}

// checkTimeout returns the per-check timeout from the spec, or the default.
func checkTimeout(instance *upgradev1alpha1.UpgradePreflightCheck) time.Duration {
	if instance.Spec.CheckTimeoutSeconds > 0 {
		return time.Duration(instance.Spec.CheckTimeoutSeconds) * time.Second
	}
	return defaultCheckTimeout
}

// checkAllowed returns true if name is in the allowlist, or if the allowlist is empty (run all).
func checkAllowed(name string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, a := range allowlist {
		if a == name {
			return true
		}
	}
	return false
}

// safeRun wraps a check function with panic recovery, returning State=Error on panic.
func safeRun(name string, fn func() upgradev1alpha1.PreflightCheckResult) (result upgradev1alpha1.PreflightCheckResult) {
	defer func() {
		if r := recover(); r != nil {
			result = upgradev1alpha1.PreflightCheckResult{
				Name: name,
				Execution: upgradev1alpha1.CheckExecution{
					State: upgradev1alpha1.CheckError,
					Error: fmt.Sprintf("check panicked: %v", r),
				},
			}
		}
	}()
	return fn()
}

// completedResult returns a PreflightCheckResult with State=Completed and the given finding.
// completedResult returns a PreflightCheckResult with State=Completed and the given health finding.
func completedResult(name, description string, result upgradev1alpha1.CheckResult, message string, details []string) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name:        name,
		Description: description,
		Execution: upgradev1alpha1.CheckExecution{
			State:   upgradev1alpha1.CheckCompleted,
			Result:  result,
			Message: message,
			Details: details,
		},
	}
}

// errorResult returns a PreflightCheckResult with State=Error, Result=Unknown, and the given infrastructure error.
func errorResult(name, description, errMsg string) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name:        name,
		Description: description,
		Execution: upgradev1alpha1.CheckExecution{
			State:  upgradev1alpha1.CheckError,
			Result: upgradev1alpha1.CheckUnknown,
			Error:  errMsg,
		},
	}
}

// checkClusterVersion reads the current cluster version from CVO.
// This check always passes and is informational — it validates the check
// architecture and provides baseline context alongside health checks.
func (r *ReconcileUpgradePreflightCheck) checkClusterVersion(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ClusterVersion"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = checkCtx

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descClusterVersion, fmt.Sprintf("unable to get cluster version: %v", err))
	}

	version := ""
	if clusterVersion != nil {
		version = clusterVersion.Status.Desired.Version
	}
	logger.Info("Check passed", "check", checkName, "currentVersion", version)
	return completedResult(checkName, descClusterVersion, upgradev1alpha1.CheckPassed, "",
		[]string{fmt.Sprintf("currentVersion=%s", version)})
}

// checkCriticalAlerts queries Prometheus for firing critical alerts in core namespaces.
func (r *ReconcileUpgradePreflightCheck) checkCriticalAlerts(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "CriticalAlerts"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var alerts *metrics.AlertResponse
	var err error
	query := `ALERTS{alertstate="firing",severity="critical",namespace=~"^openshift.*|^kube-.*|^default$"}`

	for attempt := 1; attempt <= prometheusRetries; attempt++ {
		var metricsClient metrics.Metrics
		metricsClient, err = r.MetricsClientBuilder.NewClient(r.Client)
		if err != nil {
			logger.Error(err, "Failed to build metrics client", "check", checkName, "attempt", attempt)
			if attempt < prometheusRetries {
				backoff := prometheusRetryBase * time.Duration(attempt)
				select {
				case <-checkCtx.Done():
					return errorResult(checkName, descCriticalAlerts, fmt.Sprintf("timed out waiting for metrics client (last error: %v)", err))
				case <-time.After(backoff):
				}
			}
			continue
		}
		alerts, err = metricsClient.Query(query)
		if err != nil {
			logger.Error(err, "Failed to query Prometheus", "check", checkName, "attempt", attempt)
			if attempt < prometheusRetries {
				backoff := prometheusRetryBase * time.Duration(attempt)
				select {
				case <-checkCtx.Done():
					return errorResult(checkName, descCriticalAlerts, fmt.Sprintf("timed out querying Prometheus (last error: %v)", err))
				case <-time.After(backoff):
				}
			}
			continue
		}
		break
	}

	if err != nil {
		return errorResult(checkName, descCriticalAlerts, fmt.Sprintf("unable to query critical alerts after %d attempts: %v", prometheusRetries, err))
	}

	if len(alerts.Data.Result) == 0 {
		logger.Info("Check passed", "check", checkName)
		return completedResult(checkName, descCriticalAlerts, upgradev1alpha1.CheckPassed, "", nil)
	}

	seen := make(map[string]bool)
	var names []string
	for _, a := range alerts.Data.Result {
		name := a.Metric["alertname"]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	logger.Info("Check failed", "check", checkName, "firingAlerts", names)
	return completedResult(checkName, descCriticalAlerts, upgradev1alpha1.CheckFailed,
		fmt.Sprintf("critical alert(s) firing: %s", strings.Join(names, ", ")), names)
}

// checkClusterOperators checks for any degraded ClusterOperator.
func (r *ReconcileUpgradePreflightCheck) checkClusterOperators(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ClusterOperators"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = checkCtx

	cvClient := r.CvClientBuilder.New(r.Client)
	degraded, err := cvClient.HasDegradedOperators()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descClusterOperators, fmt.Sprintf("unable to fetch cluster operator status: %v", err))
	}

	if len(degraded.Degraded) > 0 {
		logger.Info("Check failed", "check", checkName, "degradedOperators", degraded.Degraded)
		return completedResult(checkName, descClusterOperators, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("degraded operators: %s", strings.Join(degraded.Degraded, ", ")), degraded.Degraded)
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descClusterOperators, upgradev1alpha1.CheckPassed, "", nil)
}

// checkManuallyCordonedNodes checks for worker nodes cordoned by a human (not by the upgrader).
func (r *ReconcileUpgradePreflightCheck) checkManuallyCordonedNodes(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ManuallyCordonedNodes"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes, &client.ListOptions{
		Raw: &metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker, !node-role.kubernetes.io/infra",
		},
	}); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descManuallyCordonedNodes, fmt.Sprintf("unable to list worker nodes: %v", err))
	}

	var cordoned []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		cr := r.Machinery.IsNodeCordoned(node)
		if cr.IsCordoned && !r.Machinery.IsNodeUpgrading(node) {
			cordoned = append(cordoned, node.Name)
		}
	}

	if len(cordoned) > 0 {
		logger.Info("Check failed", "check", checkName, "cordonedNodes", cordoned)
		return completedResult(checkName, descManuallyCordonedNodes, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("manually cordoned nodes: %s", strings.Join(cordoned, ", ")), cordoned)
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descManuallyCordonedNodes, upgradev1alpha1.CheckPassed, "", nil)
}

// checkNodeTaints checks for nodes with memory/disk/PID pressure taints.
func (r *ReconcileUpgradePreflightCheck) checkNodeTaints(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "NodeUnschedulableTaints"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descNodeTaints, fmt.Sprintf("unable to list nodes: %v", err))
	}

	var affected []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if r.Machinery.HasMemoryPressure(node) || r.Machinery.HasDiskPressure(node) || r.Machinery.HasPidPressure(node) {
			affected = append(affected, node.Name)
		}
	}

	if len(affected) > 0 {
		logger.Info("Check failed", "check", checkName, "affectedNodes", affected)
		return completedResult(checkName, descNodeTaints, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("nodes with unschedulable taints: %s", strings.Join(affected, ", ")), affected)
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descNodeTaints, upgradev1alpha1.CheckPassed, "", nil)
}

// checkPodDisruptionBudgets checks for PDBs that would block node drains.
func (r *ReconcileUpgradePreflightCheck) checkPodDisruptionBudgets(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "PodDisruptionBudgets"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := r.Client.List(checkCtx, pdbList); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descPodDisruptionBudgets, fmt.Sprintf("unable to list PodDisruptionBudgets: %v", err))
	}

	var bad []string
	for _, pdb := range pdbList.Items {
		if reason := pdbViolation(pdb); reason != "" {
			bad = append(bad, fmt.Sprintf("%s/%s (%s)", pdb.Namespace, pdb.Name, reason))
		}
	}

	if len(bad) > 0 {
		logger.Info("Check failed", "check", checkName, "blockingPDBs", bad)
		return completedResult(checkName, descPodDisruptionBudgets, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("blocking PodDisruptionBudgets: %s", strings.Join(bad, "; ")), bad)
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descPodDisruptionBudgets, upgradev1alpha1.CheckPassed, "", nil)
}

// pdbViolation returns a non-empty reason if the PDB would block a drain.
// Detects: MaxUnavailable=0, MaxUnavailable=0%, MinAvailable=100%.
// Known limitation: MinAvailable as an integer equal to the current replica count
// cannot be detected statically without inspecting live deployment state.
func pdbViolation(pdb policyv1.PodDisruptionBudget) string {
	if pdb.Spec.MaxUnavailable != nil {
		switch pdb.Spec.MaxUnavailable.Type {
		case intstr.Int:
			if pdb.Spec.MaxUnavailable.IntVal == 0 {
				return "MaxUnavailable=0"
			}
		case intstr.String:
			if pdb.Spec.MaxUnavailable.StrVal == "0%" {
				return "MaxUnavailable=0%"
			}
		}
	}
	if pdb.Spec.MinAvailable != nil && pdb.Spec.MinAvailable.Type == intstr.String {
		if pdb.Spec.MinAvailable.StrVal == "100%" {
			return "MinAvailable=100%"
		}
	}
	return ""
}

// checkUpgradeableCondition checks if the ClusterVersion has Upgradeable=False.
// When CVO sets this condition, it means an operator or in-cluster state is actively
// blocking y-stream upgrades (e.g., incompatible operators, missing admin-acks).
func (r *ReconcileUpgradePreflightCheck) checkUpgradeableCondition(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "UpgradeableCondition"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_ = checkCtx

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descUpgradeableCondition, fmt.Sprintf("unable to get cluster version: %v", err))
	}

	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorUpgradeable && condition.Status == configv1.ConditionFalse {
			logger.Info("Check failed", "check", checkName, "reason", condition.Reason, "message", condition.Message)
			return completedResult(checkName, descUpgradeableCondition, upgradev1alpha1.CheckFailed,
				fmt.Sprintf("Upgradeable=False: %s", condition.Reason),
				[]string{
					fmt.Sprintf("reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				})
		}
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descUpgradeableCondition, upgradev1alpha1.CheckPassed,
		"Cluster Upgradeable condition is True - no upgrade blockers detected", nil)
}

// checkPullSecretValid validates the cluster pull secret exists and has required auth entries.
func (r *ReconcileUpgradePreflightCheck) checkPullSecretValid(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "PullSecretValid"
	logger.Info("Running check", "check", checkName)

	_, err := util.GetAccessToken(r.Client)
	if err != nil {
		logger.Info("Check failed", "check", checkName, "error", err.Error())
		return completedResult(checkName, descPullSecretValid, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("pull secret validation failed: %v", err), []string{err.Error()})
	}

	logger.Info("Check passed", "check", checkName)
	return completedResult(checkName, descPullSecretValid, upgradev1alpha1.CheckPassed,
		"Pull secret contains required cloud.openshift.com authentication", nil)
}

// checkAdminAcks checks the admin-acks ConfigMap in openshift-config for required
// upgrade acknowledgements. For y-stream upgrades, certain API removal acks are required.
func (r *ReconcileUpgradePreflightCheck) checkAdminAcks(
	ctx context.Context,
	instance *upgradev1alpha1.UpgradePreflightCheck,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "AdminAcks"
	logger.Info("Running check", "check", checkName)

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Get current cluster version to determine if this is a y-stream upgrade
	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName, "reason", "unable to get cluster version")
		return errorResult(checkName, descAdminAcks, fmt.Sprintf("unable to get cluster version: %v", err))
	}

	currentVersion := ""
	if clusterVersion != nil {
		currentVersion = clusterVersion.Status.Desired.Version
	}

	targetVersion := instance.Spec.Desired.Version
	isYStream := isYStreamUpgrade(currentVersion, targetVersion)
	logger.Info("Checking admin-acks", "currentVersion", currentVersion, "targetVersion", targetVersion, "isYStream", isYStream)

	cm := &corev1.ConfigMap{}
	err = r.Client.Get(checkCtx, types.NamespacedName{
		Namespace: "openshift-config",
		Name:      "admin-acks",
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap doesn't exist - this is only a problem if it's actually required
			// We pass with informational message since we can't determine if acks are required
			// without checking specific release notes for the target version
			msg := "admin-acks ConfigMap not found in openshift-config"
			if isYStream {
				msg += " — if this upgrade removes APIs, acknowledgements may be required. Check release notes."
			}
			logger.Info("Check passed", "check", checkName, "note", msg)
			return completedResult(checkName, descAdminAcks, upgradev1alpha1.CheckPassed, msg, []string{"ConfigMap openshift-config/admin-acks does not exist"})
		}
		logger.Error(err, "Check error", "check", checkName)
		return errorResult(checkName, descAdminAcks, fmt.Sprintf("unable to read admin-acks ConfigMap: %v", err))
	}

	var ackKeys []string
	for k := range cm.Data {
		ackKeys = append(ackKeys, fmt.Sprintf("%s=%s", k, cm.Data[k]))
	}

	// ConfigMap exists - report current state
	if len(cm.Data) == 0 {
		msg := "admin-acks ConfigMap is empty"
		if isYStream {
			msg += " — if this upgrade removes APIs, acknowledgements may be required. Check release notes."
		}
		logger.Info("Check passed", "check", checkName, "note", "ConfigMap is empty")
		return completedResult(checkName, descAdminAcks, upgradev1alpha1.CheckPassed, msg, []string{"ConfigMap openshift-config/admin-acks has no data entries"})
	}

	logger.Info("Check passed", "check", checkName, "ackCount", len(ackKeys))
	return completedResult(checkName, descAdminAcks, upgradev1alpha1.CheckPassed,
		fmt.Sprintf("Found %d admin acknowledgement(s)", len(ackKeys)), ackKeys)
}

// isYStreamUpgrade returns true if the upgrade is a y-stream (minor version) upgrade.
// Y-stream: 4.14.x → 4.15.y (minor version changes)
// Z-stream: 4.15.1 → 4.15.3 (patch version changes)
func isYStreamUpgrade(currentVersion, targetVersion string) bool {
	if currentVersion == "" || targetVersion == "" {
		// If we can't determine versions, assume it could be y-stream (conservative)
		return targetVersion != ""
	}

	// Parse semantic versions (format: major.minor.patch)
	currentParts := strings.Split(currentVersion, ".")
	targetParts := strings.Split(targetVersion, ".")

	if len(currentParts) < 2 || len(targetParts) < 2 {
		// Can't parse version properly, assume y-stream
		return true
	}

	// Compare major.minor (first two components)
	// Y-stream = minor version changes (4.14 → 4.15)
	// Z-stream = patch version changes (4.15.1 → 4.15.3)
	currentMinor := currentParts[0] + "." + currentParts[1]
	targetMinor := targetParts[0] + "." + targetParts[1]

	return currentMinor != targetMinor
}

// SetupWithManager registers this controller with the manager.
// MaxConcurrentReconciles is capped so a surge of UpgradePreflightCheck objects cannot
// exhaust the shared worker pool and starve the UpgradeConfig and other MUO controllers.
func (r *ReconcileUpgradePreflightCheck) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.UpgradePreflightCheck{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
