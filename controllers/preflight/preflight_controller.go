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
	"github.com/openshift/managed-upgrade-operator/pkg/dvo"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
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

	// maxPreflightChecks is the maximum number of UpgradePreflightCheck CRs allowed
	// cluster-wide at any time. Creating more than this returns a rejection status.
	// Delete completed CRs to make room for new ones.
	maxPreflightChecks = 10

	// preflightCheckTTL is the age after which a UpgradePreflightCheck CR is auto-deleted.
	preflightCheckTTL = 24 * time.Hour

	// deleteAfterAnnotation is set by the HTTP service to record an explicit expiry time.
	// The controller respects it in addition to the creation-timestamp-based TTL.
	deleteAfterAnnotation = "preflight.managed.openshift.io/delete-after"
)

// prometheusRetryBase is a var so tests in this package can override it to avoid slow backoff waits.
var prometheusRetryBase = 2 * time.Second

// Description constants for all preflight checks.
const (
	descClusterVersion    = "Reads the current cluster version from CVO. This is an informational check that always passes; it establishes baseline context alongside health checks."
	descClusterOperators  = "Validates that no cluster operators are degraded, using the same check MUO runs before starting an upgrade. Checks operator status via Prometheus metrics."
	descCriticalAlerts    = "Checks for critical alerts that would block an upgrade, using MUO's default alert rules. Queries Prometheus for firing critical alerts."
	descNodeHealth        = "Checks for manually cordoned worker nodes and nodes with memory, disk, or PID pressure taints that would block scheduling, using the same checks MUO runs before starting an upgrade."
	descPodDisruptionBudgets = "Checks for PDBs that block node drains, using the same HealthCheckPDB function MUO calls during upgrades. Combines direct PDB inspection with DVO metrics."
	descUpgradeableCondition = "Checks if the ClusterVersion has Upgradeable=False. When CVO sets this condition, it means an operator or in-cluster state is actively blocking y-stream upgrades."
	descPullSecretValid   = "Validates the cluster pull secret exists and has the required authentication entries for accessing Red Hat container registries."
	descAdminAcks         = "Validates admin acknowledgments for upgrade gate conditions. For y-stream upgrades, checks the openshift-config/admin-acks ConfigMap for required acknowledgements."

	descClusterOperatorsExtended  = "Comprehensive cluster operator health check. Lists all operators, their versions, available/degraded/progressing conditions, and detailed status messages. Goes beyond baseline to show full operator state."
	descCriticalAlertsExtended    = "Detailed critical alert analysis without ignore filters. Lists all firing critical alerts with alertname in core OpenShift namespaces (openshift-*, kube-*, default)."
	descNodeHealthExtended        = "In-depth node health analysis. Shows all worker nodes that are cordoned (not being upgraded) or have memory, disk, or PID pressure conditions that could block workload scheduling."
	descPodDisruptionBudgetsExtended = "Comprehensive PDB analysis beyond baseline check. Lists all PDBs across all namespaces with MaxUnavailable=0, MaxUnavailable=0%, or MinAvailable=100% configurations that could block node drains."
)

// allChecks is the ordered list of check names run when no allowlist is specified.
var allChecks = []string{
	"ClusterVersion",
	"ClusterOperators",             // Baseline: calls upgraders.ClusterOperators()
	"ClusterOperatorsExtended",     // Comprehensive: checks all degraded operators
	"CriticalAlerts",               // Baseline: calls upgraders.CriticalAlertsDefault()
	"CriticalAlertsExtended",       // Comprehensive: direct Prometheus query with retry
	"PodDisruptionBudgets",         // Baseline: calls upgraders.HealthCheckPDB() (includes DVO metrics)
	"PodDisruptionBudgetsExtended", // Comprehensive: cluster-wide PDB drain-block detection
	"ManuallyCordonedNodes",        // Baseline: calls upgraders.ManuallyCordonedNodes()
	"ManuallyCordonedNodesExtended", // Comprehensive: all cordoned worker nodes
	"NodeUnschedulableTaints",      // Baseline: calls upgraders.NodeUnschedulableTaints()
	"NodeUnschedulableTaintsExtended", // Comprehensive: memory/disk/PID pressure taints
	"UpgradeableCondition",         // Standalone
	"PullSecretValid",              // Standalone
	"AdminAcks",                    // Standalone
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
	DvoClientBuilder     dvo.DvoClientBuilder
}

// pruneAndCount deletes expired UpgradePreflightCheck CRs (by TTL or delete-after annotation)
// and returns the total number of remaining CRs in the namespace (excluding skipName).
// Errors deleting individual CRs are logged but do not stop the scan.
func (r *ReconcileUpgradePreflightCheck) pruneAndCount(ctx context.Context, namespace, skipName string, logger logr.Logger) (int, error) {
	list := &upgradev1alpha1.UpgradePreflightCheckList{}
	if err := r.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("listing UpgradePreflightChecks: %w", err)
	}
	now := time.Now()
	remaining := 0
	for i := range list.Items {
		cr := &list.Items[i]
		expired := cr.CreationTimestamp.Time.Add(preflightCheckTTL).Before(now)
		if ann := cr.Annotations[deleteAfterAnnotation]; ann != "" {
			if t, err := time.Parse(time.RFC3339, ann); err == nil && t.Before(now) {
				expired = true
			}
		}
		if expired && cr.Name != skipName {
			logger.Info("Pruning expired UpgradePreflightCheck", "name", cr.Name,
				"age", now.Sub(cr.CreationTimestamp.Time).Round(time.Minute))
			if err := r.Client.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to prune expired UpgradePreflightCheck", "name", cr.Name)
			}
			continue
		}
		remaining++
	}
	return remaining, nil
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

	// Prune expired CRs and enforce the cluster-wide limit.
	// This runs after the completed-check so already-finished CRs skip it on requeue.
	if total, listErr := r.pruneAndCount(ctx, req.Namespace, instance.Name, logger); listErr != nil {
		logger.Error(listErr, "Could not list UpgradePreflightChecks; skipping limit enforcement")
	} else if total >= maxPreflightChecks {
		logger.Info("UpgradePreflightCheck limit reached; marking CR as rejected",
			"limit", maxPreflightChecks, "current", total, "name", instance.Name)
		now := metav1.NewTime(time.Now())
		if instance.Status.StartedAt == nil {
			instance.Status.StartedAt = &now
		}
		instance.Status.CompletedAt = &now
		instance.Status.Checks = []upgradev1alpha1.PreflightCheckResult{{
			Name: "LimitCheck",
			Execution: upgradev1alpha1.CheckExecution{
				State: upgradev1alpha1.CheckError,
				Error: fmt.Sprintf(
					"too many UpgradePreflightCheck CRs (%d/%d). "+
						"Delete completed checks before creating new ones. "+
						"Checks older than 24 hours are pruned automatically.",
					total, maxPreflightChecks),
			},
		}}
		if err := r.Client.Status().Update(ctx, instance); err != nil {
			logger.Error(err, "Failed to write rejection status to UpgradePreflightCheck")
		}
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
		"ClusterVersion": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkClusterVersion(ctx, logger, timeout)
		},
		"ClusterOperators": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkClusterOperators(ctx, logger, timeout)
		},
		"ClusterOperatorsExtended": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkClusterOperatorsExtended(ctx, logger, timeout)
		},
		"CriticalAlerts": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkCriticalAlerts(ctx, logger, timeout)
		},
		"CriticalAlertsExtended": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkCriticalAlertsExtended(ctx, logger, timeout)
		},
		"PodDisruptionBudgets": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkPodDisruptionBudgets(ctx, logger, timeout)
		},
		"PodDisruptionBudgetsExtended": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkPodDisruptionBudgetsExtended(ctx, logger, timeout)
		},
		"ManuallyCordonedNodes": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkManuallyCordonedNodes(ctx, logger, timeout)
		},
		"ManuallyCordonedNodesExtended": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkManuallyCordonedNodesExtended(ctx, logger, timeout)
		},
		"NodeUnschedulableTaints": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkNodeTaints(ctx, logger, timeout)
		},
		"NodeUnschedulableTaintsExtended": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkNodeTaintsExtended(ctx, logger, timeout)
		},
		"UpgradeableCondition": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkUpgradeableCondition(ctx, logger, timeout)
		},
		"PullSecretValid": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkPullSecretValid(ctx, logger, timeout)
		},
		"AdminAcks": func() upgradev1alpha1.PreflightCheckResult {
			return r.checkAdminAcks(ctx, instance, logger, timeout)
		},
	}

	var results []upgradev1alpha1.PreflightCheckResult
	for _, name := range allChecks {
		if !checkAllowed(name, enabled) {
			logger.V(4).Info("Skipping check (not in allowlist)", "check", name)
			results = append(results, upgradev1alpha1.PreflightCheckResult{
				Name:      name,
				Execution: upgradev1alpha1.CheckExecution{State: upgradev1alpha1.CheckSkipped},
			})
			continue
		}
		fn := dispatch[name]
		if fn == nil {
			logger.Error(nil, "Check has no registered implementation; skipping", "check", name)
			results = append(results, errorResult(name, fmt.Sprintf("check %q has no registered implementation — this is a controller bug", name)))
			continue
		}
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
func completedResult(name string, finding upgradev1alpha1.CheckFinding) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name:      name,
		Execution: upgradev1alpha1.CheckExecution{State: upgradev1alpha1.CheckCompleted},
		Result:    &finding,
	}
}

// errorResult returns a PreflightCheckResult with State=Error and the given infrastructure error.
func errorResult(name, errMsg string) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name: name,
		Execution: upgradev1alpha1.CheckExecution{
			State: upgradev1alpha1.CheckError,
			Error: errMsg,
		},
	}
}

// withDesc attaches a description to a check result and returns it.
func withDesc(r upgradev1alpha1.PreflightCheckResult, desc string) upgradev1alpha1.PreflightCheckResult {
	r.Description = desc
	return r
}

// createFakeUpgradeConfig creates a minimal UpgradeConfig for calling baseline upgrade validation functions.
func createFakeUpgradeConfig(targetVersion string) *upgradev1alpha1.UpgradeConfig {
	return &upgradev1alpha1.UpgradeConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "preflight-check",
		},
		Spec: upgradev1alpha1.UpgradeConfigSpec{
			Desired: upgradev1alpha1.Update{
				Version: targetVersion,
			},
		},
		Status: upgradev1alpha1.UpgradeConfigStatus{
			History: upgradev1alpha1.UpgradeHistories{
				{
					Version: targetVersion,
					Phase:   upgradev1alpha1.UpgradePhaseNew,
				},
			},
		},
	}
}

// applyTiming sets per-check timing fields on the named return value via defer.
// Usage: defer applyCheckTiming(&result, startTime, timeoutSec, &execMsg, &execDetails)()
// (not used directly — timing is applied inline via defer closures per function)

// checkClusterVersion reads the current cluster version from CVO.
// This check always passes and is informational — it validates the check
// architecture and provides baseline context alongside health checks.
func (r *ReconcileUpgradePreflightCheck) checkClusterVersion(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "ClusterVersion"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to read cluster version"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to get cluster version: %v", err)), descClusterVersion)
	}

	version := ""
	if clusterVersion != nil {
		version = clusterVersion.Status.Desired.Version
	}
	logger.Info("Check passed", "check", checkName, "currentVersion", version)
	execMsg = fmt.Sprintf("Cluster version %s", version)
	execDetails = []string{fmt.Sprintf("currentVersion=%s", version)}
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: []string{fmt.Sprintf("currentVersion=%s", version)},
	}), descClusterVersion)
}

// checkClusterOperators is a baseline wrapper that calls upgraders.ClusterOperators(),
// the exact function the upgrade process uses for cluster operator validation.
func (r *ReconcileUpgradePreflightCheck) checkClusterOperators(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "ClusterOperators"
	logger.Info("Running baseline check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		execMsg = "unable to build metrics client"
		return withDesc(errorResult(checkName, fmt.Sprintf("Baseline check: failed to create metrics client: %v", err)), descClusterOperators)
	}

	execDetails = []string{"Uses MUO's upgrade validation logic (upgraders.ClusterOperators)"}

	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.ClusterOperators(metricsClient, r.CvClientBuilder.New(r.Client), fakeUC, logger, "")
	if err != nil {
		execMsg = "upgrade validation failed: degraded operators detected"
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("Baseline check: degraded operators detected: %v", err),
			Details: []string{"This check uses the same validation the upgrade process uses"},
		}), descClusterOperators)
	}
	if !passed {
		execMsg = "upgrade validation failed: degraded operators detected"
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: "Baseline check: degraded operators detected (upgraders.ClusterOperators)",
			Details: []string{"This check uses the same validation the upgrade process uses"},
		}), descClusterOperators)
	}

	logger.Info("Baseline check passed", "check", checkName)
	execMsg = "Upgrade validation passed: no degraded operators detected"
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: []string{"This check uses the same validation the upgrade process uses"},
	}), descClusterOperators)
}

// checkClusterOperatorsExtended checks for any degraded ClusterOperator and lists all operators with their status.
func (r *ReconcileUpgradePreflightCheck) checkClusterOperatorsExtended(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "ClusterOperatorsExtended"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// List all ClusterOperators for rich per-operator details.
	coList := &configv1.ClusterOperatorList{}
	if err := r.Client.List(checkCtx, coList); err == nil {
		for _, co := range coList.Items {
			availStatus := "Unknown"
			degradedStatus := "Unknown"
			for _, cond := range co.Status.Conditions {
				if cond.Type == configv1.OperatorAvailable {
					availStatus = string(cond.Status)
				}
				if cond.Type == configv1.OperatorDegraded {
					degradedStatus = string(cond.Status)
				}
			}
			execDetails = append(execDetails, fmt.Sprintf("%s: Available=%s, Degraded=%s", co.Name, availStatus, degradedStatus))
		}
	}

	cvClient := r.CvClientBuilder.New(r.Client)
	degraded, err := cvClient.HasDegradedOperators()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to fetch cluster operator status"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to fetch cluster operator status: %v", err)), descClusterOperatorsExtended)
	}

	if len(degraded.Degraded) > 0 {
		logger.Info("Check failed", "check", checkName, "degradedOperators", degraded.Degraded)
		execMsg = fmt.Sprintf("%d degraded operator(s): %s", len(degraded.Degraded), strings.Join(degraded.Degraded, ", "))
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("degraded operators: %s", strings.Join(degraded.Degraded, ", ")),
			Details: degraded.Degraded,
		}), descClusterOperatorsExtended)
	}

	logger.Info("Check passed", "check", checkName)
	if len(coList.Items) > 0 {
		execMsg = fmt.Sprintf("All %d cluster operators healthy", len(coList.Items))
	} else {
		execMsg = "No degraded operators detected"
	}
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descClusterOperatorsExtended)
}

// checkCriticalAlerts is a baseline wrapper that calls upgraders.CriticalAlertsDefault(),
// the exact function the upgrade process uses for critical alert validation.
func (r *ReconcileUpgradePreflightCheck) checkCriticalAlerts(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "CriticalAlerts"
	logger.Info("Running baseline check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		execMsg = "unable to build metrics client"
		return withDesc(errorResult(checkName, fmt.Sprintf("Baseline check: failed to create metrics client: %v", err)), descCriticalAlerts)
	}

	execDetails = []string{"Uses MUO's upgrade validation logic (upgraders.CriticalAlertsDefault)"}

	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.CriticalAlertsDefault(metricsClient, fakeUC, logger, "")
	if err != nil {
		execMsg = "upgrade validation failed: critical alerts detected"
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("Baseline check: critical alerts firing: %v", err),
			Details: []string{"This check uses the same validation the upgrade process uses, including ignore lists"},
		}), descCriticalAlerts)
	}
	if !passed {
		execMsg = "upgrade validation failed: critical alerts detected"
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: "Baseline check: critical alerts firing (upgraders.CriticalAlertsDefault)",
			Details: []string{"This check uses the same validation the upgrade process uses, including ignore lists"},
		}), descCriticalAlerts)
	}

	logger.Info("Baseline check passed", "check", checkName)
	execMsg = "Upgrade validation passed: no critical alerts firing"
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: []string{"This check uses the same validation the upgrade process uses, including ignore lists"},
	}), descCriticalAlerts)
}

// checkCriticalAlertsExtended queries Prometheus for firing critical alerts in core namespaces.
func (r *ReconcileUpgradePreflightCheck) checkCriticalAlertsExtended(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "CriticalAlertsExtended"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

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
					execMsg = "timed out waiting for metrics client"
					return withDesc(errorResult(checkName, fmt.Sprintf("timed out waiting for metrics client (last error: %v)", err)), descCriticalAlertsExtended)
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
					execMsg = "timed out querying Prometheus"
					return withDesc(errorResult(checkName, fmt.Sprintf("timed out querying Prometheus (last error: %v)", err)), descCriticalAlertsExtended)
				case <-time.After(backoff):
				}
			}
			continue
		}
		break
	}

	if err != nil {
		execMsg = fmt.Sprintf("unable to query critical alerts after %d attempts", prometheusRetries)
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to query critical alerts after %d attempts: %v", prometheusRetries, err)), descCriticalAlertsExtended)
	}

	if len(alerts.Data.Result) == 0 {
		logger.Info("Check passed", "check", checkName)
		execMsg = "No critical alerts firing in core namespaces"
		execDetails = []string{"No firing critical alerts found in openshift-*, kube-*, or default namespaces"}
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descCriticalAlertsExtended)
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
	execMsg = fmt.Sprintf("%d critical alert(s) firing: %s", len(names), strings.Join(names, ", "))
	execDetails = names
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckFailed,
		Message: fmt.Sprintf("critical alert(s) firing: %s", strings.Join(names, ", ")),
		Details: names,
	}), descCriticalAlertsExtended)
}

// checkManuallyCordonedNodes is a baseline wrapper that calls upgraders.ManuallyCordonedNodes(),
// the exact function the upgrade process uses for cordoned node validation.
func (r *ReconcileUpgradePreflightCheck) checkManuallyCordonedNodes(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "ManuallyCordonedNodes"
	logger.Info("Running baseline check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		execMsg = "unable to build metrics client"
		return withDesc(errorResult(checkName, fmt.Sprintf("Baseline check: failed to create metrics client: %v", err)), descNodeHealth)
	}

	execDetails = []string{"Uses MUO's upgrade validation logic (upgraders.ManuallyCordonedNodes)"}

	fakeUC := createFakeUpgradeConfig("4.15.0")
	cordonedNodes, err := upgraders.ManuallyCordonedNodes(metricsClient, r.Machinery, r.Client, fakeUC, logger, "")
	if err != nil && len(cordonedNodes) == 0 {
		execMsg = "unable to check cordoned nodes"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to check cordoned nodes: %v", err)), descNodeHealth)
	}
	if len(cordonedNodes) > 0 {
		details := []string{"This check uses the same validation the upgrade process uses, worker nodes only"}
		for _, node := range cordonedNodes {
			details = append(details, fmt.Sprintf("cordoned worker node: %s", node))
		}
		execMsg = fmt.Sprintf("Upgrade validation failed: %d manually cordoned worker node(s)", len(cordonedNodes))
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("Baseline check: %d manually cordoned worker node(s)", len(cordonedNodes)),
			Details: details,
		}), descNodeHealth)
	}

	logger.Info("Baseline check passed", "check", checkName)
	execMsg = "Upgrade validation passed: no manually cordoned nodes"
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: []string{"This check uses the same validation the upgrade process uses, worker nodes only"},
	}), descNodeHealth)
}

// checkManuallyCordonedNodesExtended checks for worker nodes cordoned by a human (not by the upgrader).
func (r *ReconcileUpgradePreflightCheck) checkManuallyCordonedNodesExtended(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "ManuallyCordonedNodesExtended"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes, &client.ListOptions{
		Raw: &metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/worker, !node-role.kubernetes.io/infra",
		},
	}); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to list worker nodes"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to list worker nodes: %v", err)), descNodeHealthExtended)
	}

	var cordoned []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		cr := r.Machinery.IsNodeCordoned(node)
		if cr.IsCordoned && !r.Machinery.IsNodeUpgrading(node) {
			cordoned = append(cordoned, node.Name)
			execDetails = append(execDetails, fmt.Sprintf("%s: manually cordoned", node.Name))
		} else {
			execDetails = append(execDetails, fmt.Sprintf("%s: schedulable", node.Name))
		}
	}

	if len(cordoned) > 0 {
		logger.Info("Check failed", "check", checkName, "cordonedNodes", cordoned)
		execMsg = fmt.Sprintf("%d manually cordoned node(s) of %d worker node(s)", len(cordoned), len(nodes.Items))
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("manually cordoned nodes: %s", strings.Join(cordoned, ", ")),
			Details: cordoned,
		}), descNodeHealthExtended)
	}

	logger.Info("Check passed", "check", checkName)
	execMsg = fmt.Sprintf("All %d worker node(s) are schedulable", len(nodes.Items))
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descNodeHealthExtended)
}

// checkNodeTaints is a baseline wrapper that calls upgraders.NodeUnschedulableTaints(),
// the exact function the upgrade process uses for node taint validation.
func (r *ReconcileUpgradePreflightCheck) checkNodeTaints(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "NodeUnschedulableTaints"
	logger.Info("Running baseline check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		execMsg = "unable to build metrics client"
		return withDesc(errorResult(checkName, fmt.Sprintf("Baseline check: failed to create metrics client: %v", err)), descNodeHealth)
	}

	execDetails = []string{"Uses MUO's upgrade validation logic (upgraders.NodeUnschedulableTaints)"}

	fakeUC := createFakeUpgradeConfig("4.15.0")
	taintedNodes, err := upgraders.NodeUnschedulableTaints(metricsClient, r.Machinery, r.Client, fakeUC, logger, "")
	if err != nil && len(taintedNodes) == 0 {
		execMsg = "unable to check node taints"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to check node taints: %v", err)), descNodeHealth)
	}
	if len(taintedNodes) > 0 {
		details := []string{"This check uses the same validation the upgrade process uses, checks memory/disk/PID pressure taints"}
		for _, node := range taintedNodes {
			details = append(details, fmt.Sprintf("node with pressure taint: %s", node))
		}
		execMsg = fmt.Sprintf("Upgrade validation failed: %d node(s) with pressure taints", len(taintedNodes))
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("Baseline check: %d node(s) with pressure taints", len(taintedNodes)),
			Details: details,
		}), descNodeHealth)
	}

	logger.Info("Baseline check passed", "check", checkName)
	execMsg = "Upgrade validation passed: no pressure taints detected"
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: []string{"This check uses the same validation the upgrade process uses, checks memory/disk/PID pressure taints"},
	}), descNodeHealth)
}

// checkNodeTaintsExtended checks for nodes with memory/disk/PID pressure taints.
func (r *ReconcileUpgradePreflightCheck) checkNodeTaintsExtended(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "NodeUnschedulableTaintsExtended"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to list nodes"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to list nodes: %v", err)), descNodeHealthExtended)
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
		execMsg = fmt.Sprintf("%d node(s) with pressure taints of %d total", len(affected), len(nodes.Items))
		execDetails = append(execDetails, fmt.Sprintf("Total nodes checked: %d", len(nodes.Items)))
		for _, name := range affected {
			execDetails = append(execDetails, fmt.Sprintf("%s: unschedulable pressure taint", name))
		}
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("nodes with unschedulable taints: %s", strings.Join(affected, ", ")),
			Details: affected,
		}), descNodeHealthExtended)
	}

	logger.Info("Check passed", "check", checkName)
	execMsg = fmt.Sprintf("All %d node(s) checked: no pressure taints", len(nodes.Items))
	execDetails = []string{fmt.Sprintf("Checked %d node(s) for memory, disk, and PID pressure — all healthy", len(nodes.Items))}
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descNodeHealthExtended)
}

// checkPodDisruptionBudgets is a baseline wrapper that calls upgraders.HealthCheckPDB(),
// the exact function the upgrade process uses for PDB validation.
func (r *ReconcileUpgradePreflightCheck) checkPodDisruptionBudgets(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "PodDisruptionBudgets"
	logger.Info("Running baseline check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	if r.DvoClientBuilder == nil {
		execMsg = "DvoClientBuilder is not configured"
		return withDesc(errorResult(checkName, "DvoClientBuilder is not configured — redeploy the operator to fix this"), descPodDisruptionBudgets)
	}

	// Respect the per-check timeout: bail early if the reconcile context is already done.
	// Note: upgraders.HealthCheckPDB does not accept a context, so sub-operations within
	// it use context.Background() internally. The timeout here guards the operations we control.
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if checkCtx.Err() != nil {
		execMsg = "context done before check could start"
		return withDesc(errorResult(checkName, fmt.Sprintf("context done before check could start: %v", checkCtx.Err())), descPodDisruptionBudgets)
	}

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		execMsg = "unable to build metrics client"
		return withDesc(errorResult(checkName, fmt.Sprintf("Baseline check: unable to build metrics client: %v", err)), descPodDisruptionBudgets)
	}

	execDetails = []string{"Uses MUO's upgrade validation logic (upgraders.HealthCheckPDB)"}

	fakeUC := createFakeUpgradeConfig("4.15.0")
	_, passed, err := upgraders.HealthCheckPDB(metricsClient, r.Client, r.DvoClientBuilder, fakeUC, logger, fakeUC.Spec.Desired.Version)
	if err != nil {
		execMsg = "PodDisruptionBudget check failed"
		return withDesc(errorResult(checkName, fmt.Sprintf("PodDisruptionBudget check failed: %v", err)), descPodDisruptionBudgets)
	}
	if !passed {
		execMsg = "Upgrade validation failed: blocking PDBs or invalid PDB configuration detected"
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: "blocking PodDisruptionBudgets or DVO metrics indicate invalid PDB configuration",
		}), descPodDisruptionBudgets)
	}

	logger.Info("Baseline check passed", "check", checkName)
	execMsg = "Upgrade validation passed: no blocking PDBs"
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descPodDisruptionBudgets)
}

func (r *ReconcileUpgradePreflightCheck) checkPodDisruptionBudgetsExtended(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "PodDisruptionBudgetsExtended"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := r.Client.List(checkCtx, pdbList); err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to list PodDisruptionBudgets"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to list PodDisruptionBudgets: %v", err)), descPodDisruptionBudgetsExtended)
	}

	var bad []string
	for _, pdb := range pdbList.Items {
		if reason := pdbViolation(pdb); reason != "" {
			entry := fmt.Sprintf("%s/%s (%s)", pdb.Namespace, pdb.Name, reason)
			bad = append(bad, entry)
			execDetails = append(execDetails, fmt.Sprintf("%s: BLOCKING — %s", fmt.Sprintf("%s/%s", pdb.Namespace, pdb.Name), reason))
		} else {
			execDetails = append(execDetails, fmt.Sprintf("%s/%s: ok", pdb.Namespace, pdb.Name))
		}
	}

	if len(bad) > 0 {
		logger.Info("Check failed", "check", checkName, "blockingPDBs", bad)
		execMsg = fmt.Sprintf("%d blocking PDB(s) of %d total", len(bad), len(pdbList.Items))
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("blocking PodDisruptionBudgets: %s", strings.Join(bad, "; ")),
			Details: bad,
		}), descPodDisruptionBudgetsExtended)
	}

	logger.Info("Check passed", "check", checkName)
	execMsg = fmt.Sprintf("Checked %d PDB(s): none blocking node drains", len(pdbList.Items))
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descPodDisruptionBudgetsExtended)
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
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "UpgradeableCondition"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to read cluster version"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to get cluster version: %v", err)), descUpgradeableCondition)
	}

	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorUpgradeable && condition.Status == configv1.ConditionFalse {
			logger.Info("Check failed", "check", checkName, "reason", condition.Reason, "message", condition.Message)
			execMsg = fmt.Sprintf("Cluster not upgradeable: %s", condition.Reason)
			execDetails = []string{
				fmt.Sprintf("Upgradeable=False: reason=%s", condition.Reason),
				fmt.Sprintf("message=%s", condition.Message),
			}
			return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
				Verdict: upgradev1alpha1.CheckFailed,
				Message: fmt.Sprintf("Upgradeable=False: %s", condition.Reason),
				Details: []string{
					fmt.Sprintf("reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				},
			}), descUpgradeableCondition)
		}
	}

	logger.Info("Check passed", "check", checkName)
	execMsg = "Cluster is upgradeable"
	execDetails = []string{"Upgradeable condition is True (or not set)"}
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descUpgradeableCondition)
}

// checkPullSecretValid validates the cluster pull secret exists and has required auth entries.
func (r *ReconcileUpgradePreflightCheck) checkPullSecretValid(
	ctx context.Context,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "PullSecretValid"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	_, err := util.GetAccessToken(r.Client)
	if err != nil {
		logger.Info("Check failed", "check", checkName, "error", err.Error())
		execMsg = "pull secret validation failed"
		execDetails = []string{err.Error()}
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: fmt.Sprintf("pull secret validation failed: %v", err),
			Details: []string{err.Error()},
		}), descPullSecretValid)
	}

	logger.Info("Check passed", "check", checkName)
	execMsg = "Pull secret is valid"
	execDetails = []string{"cloud.openshift.com auth entry is present in the cluster pull secret"}
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{Verdict: upgradev1alpha1.CheckPassed}), descPullSecretValid)
}

// checkAdminAcks checks the admin-acks ConfigMap in openshift-config for required
// upgrade acknowledgements. For y-stream upgrades, certain API removal acks are required.
func (r *ReconcileUpgradePreflightCheck) checkAdminAcks(
	ctx context.Context,
	instance *upgradev1alpha1.UpgradePreflightCheck,
	logger logr.Logger,
	timeout time.Duration,
) (result upgradev1alpha1.PreflightCheckResult) {
	const checkName = "AdminAcks"
	logger.Info("Running check", "check", checkName)

	startTime := metav1.Now()
	timeoutSec := int32(timeout.Seconds())
	var execMsg string
	var execDetails []string
	defer func() {
		completedTime := metav1.Now()
		result.Execution.StartedAt = &startTime
		result.Execution.CompletedAt = &completedTime
		result.Execution.TimeoutSeconds = timeoutSec
		result.Execution.Message = execMsg
		result.Execution.Details = execDetails
	}()

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targetVersion := instance.Spec.Desired.Version
	isYStream := isYStreamUpgrade(targetVersion)

	cm := &corev1.ConfigMap{}
	err := r.Client.Get(checkCtx, types.NamespacedName{
		Namespace: "openshift-config",
		Name:      "admin-acks",
	}, cm)

	if err != nil {
		if apierrors.IsNotFound(err) {
			if isYStream {
				logger.Info("Check failed", "check", checkName, "reason", "admin-acks ConfigMap not found for y-stream upgrade")
				execMsg = "admin-acks ConfigMap not found — required for y-stream upgrades"
				execDetails = []string{"ConfigMap openshift-config/admin-acks does not exist"}
				return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
					Verdict: upgradev1alpha1.CheckFailed,
					Message: "admin-acks ConfigMap not found in openshift-config — required for y-stream upgrades",
					Details: []string{"ConfigMap openshift-config/admin-acks does not exist"},
				}), descAdminAcks)
			}
			logger.Info("Check passed", "check", checkName, "note", "admin-acks ConfigMap not found (z-stream, not required)")
			execMsg = "Admin acks not required for z-stream upgrade"
			execDetails = []string{"ConfigMap openshift-config/admin-acks does not exist (not required for z-stream)"}
			return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
				Verdict: upgradev1alpha1.CheckPassed,
				Details: []string{"admin-acks ConfigMap not found (not required for z-stream)"},
			}), descAdminAcks)
		}
		logger.Error(err, "Check error", "check", checkName)
		execMsg = "unable to read admin-acks ConfigMap"
		return withDesc(errorResult(checkName, fmt.Sprintf("unable to read admin-acks ConfigMap: %v", err)), descAdminAcks)
	}

	var ackKeys []string
	for k := range cm.Data {
		ackKeys = append(ackKeys, fmt.Sprintf("%s=%s", k, cm.Data[k]))
	}

	if isYStream && len(cm.Data) == 0 {
		logger.Info("Check failed", "check", checkName, "reason", "admin-acks ConfigMap is empty for y-stream upgrade")
		execMsg = "admin-acks ConfigMap exists but has no entries"
		execDetails = []string{"ConfigMap openshift-config/admin-acks has no data entries"}
		return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
			Verdict: upgradev1alpha1.CheckFailed,
			Message: "admin-acks ConfigMap exists but has no entries — y-stream upgrades typically require acknowledgements",
			Details: []string{"ConfigMap openshift-config/admin-acks has no data entries"},
		}), descAdminAcks)
	}

	logger.Info("Check passed", "check", checkName, "ackCount", len(ackKeys))
	execMsg = fmt.Sprintf("Admin acks present: %d entry/entries in openshift-config/admin-acks", len(ackKeys))
	execDetails = ackKeys
	return withDesc(completedResult(checkName, upgradev1alpha1.CheckFinding{
		Verdict: upgradev1alpha1.CheckPassed,
		Details: ackKeys,
	}), descAdminAcks)
}

// isYStreamUpgrade returns true if the target version string looks like it could
// be a y-stream (minor version) upgrade. Without the current cluster version to compare,
// this is a best-effort check — it returns true for any non-empty target version,
// since admin-acks are cheap to check regardless.
func isYStreamUpgrade(targetVersion string) bool {
	return targetVersion != ""
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
