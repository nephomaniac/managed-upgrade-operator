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
	appsv1 "k8s.io/api/apps/v1"
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

	maxPreflightChecks    = 10
	preflightCheckTTL     = 24 * time.Hour
	deleteAfterAnnotation = "preflight.managed.openshift.io/delete-after"
)

var prometheusRetryBase = 2 * time.Second

const (
	descClusterVersion           = "Reads the current cluster version from CVO. This is an informational check that always passes; it establishes baseline context alongside health checks."
	descClusterOperators         = "Validates that no cluster operators are degraded, using the same check MUO runs before starting an upgrade. Checks operator status via Prometheus metrics."
	descCriticalAlerts           = "Checks for critical alerts that would block an upgrade, using MUO's default alert rules. Queries Prometheus for firing critical alerts."
	descNodeHealth               = "Checks for manually cordoned worker nodes and nodes with memory, disk, or PID pressure taints that would block scheduling, using the same checks MUO runs before starting an upgrade."
	descPodDisruptionBudgets     = "Checks for PDBs that block node drains, using the same HealthCheckPDB function MUO calls during upgrades. Combines direct PDB inspection with DVO metrics."
	descUpgradeableCondition     = "Checks if the ClusterVersion has Upgradeable=False. When CVO sets this condition, it means an operator or in-cluster state is actively blocking y-stream upgrades."
	descPullSecretValid          = "Validates the cluster pull secret exists and has the required authentication entries for accessing Red Hat container registries."
	descAdminAcks                = "Validates admin acknowledgments for upgrade gate conditions. For y-stream upgrades, checks the openshift-config/admin-acks ConfigMap for required acknowledgements."
	descClusterOperatorsExtended = "Comprehensive cluster operator health check. Lists all operators with available/degraded conditions and detailed status messages."
	descCriticalAlertsExtended   = "Detailed critical alert analysis without ignore filters. Lists all firing critical alerts in core OpenShift namespaces (openshift-*, kube-*, default)."
	descNodeHealthExtended       = "In-depth node health analysis. Shows all worker nodes that are cordoned or have memory, disk, or PID pressure conditions that could block workload scheduling."
	descUpgradeInFlight = "Checks whether a cluster version upgrade is currently in progress by inspecting the ClusterVersion Progressing condition. Starting a new upgrade while one is already in-flight can cause conflicts and unpredictable behaviour."
	descPDBMasterInfra  = "Identifies PodDisruptionBudgets that could block master or infra node drain during an upgrade. Scans two sources: (1) pods currently running on master/infra nodes, and (2) DaemonSet pod templates that tolerate master/infra taints and will always be present on those nodes. Only flags PDBs that are restrictive (MaxUnavailable=0 or MinAvailable=100%) and are confirmed to cover workloads on those nodes."
)

// ocmOnlyChecks lists checks that require OCM API access and therefore cannot run
// inside MUO. These should be implemented in an external OCM-side eligibility layer
// (e.g. ROSA CLI, OCM tooling). Documented here so they are not mistakenly re-implemented
// in-cluster without the necessary OCM client support.
//
//   - LIMITED_SUPPORT: Whether the cluster is in Limited Support; OCM-only state.
//   - NO_TARGETS_AVAILABLE / AT_LATEST_VERSION: ROSA-enabled upgrade graph; OCM owns the
//     list of ROSA-approved versions. The CVO's availableUpdates field is a partial proxy
//     but does not reflect ROSA enablement gating.
//   - TARGET_NOT_AVAILABLE: Whether a requested version exists in the ROSA upgrade graph.
//   - GATE_UNACKED (OCM-side gates): Version gate agreements are stored in OCM. The
//     AdminAcks check covers the in-cluster admin-acks ConfigMap; OCM gate agreements
//     require the OCM API (ListGateAgreements).
var _ = "ocmOnlyChecks — see comment above"

var allChecks = []string{
	"ClusterVersion",
	"UpgradeInFlight",
	"ClusterOperators",
	"ClusterOperatorsExtended",
	"CriticalAlerts",
	"CriticalAlertsExtended",
	"PodDisruptionBudgets",
	"PodDisruptionBudgetsMasterInfraExtended",
	"ManuallyCordonedNodes",
	"ManuallyCordonedNodesExtended",
	"NodeUnschedulableTaints",
	"NodeUnschedulableTaintsExtended",
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
type ReconcileUpgradePreflightCheck struct {
	Client               client.Client
	Scheme               *runtime.Scheme
	MetricsClientBuilder metrics.MetricsBuilder
	CvClientBuilder      cv.ClusterVersionBuilder
	Machinery            machinery.Machinery
	ConfigManagerBuilder configmanager.ConfigManagerBuilder
	DvoClientBuilder     dvo.DvoClientBuilder
}

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
			logger.Info("Pruning expired UpgradePreflightCheck", "name", cr.Name)
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

	defer func() {
		if p := recover(); p != nil {
			logger.Error(fmt.Errorf("%v", p), "panic recovered in preflight reconcile")
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

	if total, listErr := r.pruneAndCount(ctx, req.Namespace, instance.Name, logger); listErr != nil {
		logger.Error(listErr, "Could not list UpgradePreflightChecks; skipping limit enforcement")
	} else if total >= maxPreflightChecks {
		now := metav1.NewTime(time.Now())
		if instance.Status.StartedAt == nil {
			instance.Status.StartedAt = &now
		}
		instance.Status.CompletedAt = &now
		instance.Status.Checks = []upgradev1alpha1.PreflightCheckResult{{
			Name:           "LimitCheck",
			Result:         upgradev1alpha1.CheckResultError,
			TimeoutSeconds: 0,
			Execution: upgradev1alpha1.CheckExecution{
				State: upgradev1alpha1.CheckError,
				Error: fmt.Sprintf("too many UpgradePreflightCheck CRs (%d/%d). Delete completed checks before creating new ones.", total, maxPreflightChecks),
			},
		}}
		if err := r.Client.Status().Update(ctx, instance); err != nil {
			logger.Error(err, "Failed to write rejection status")
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
		"ClusterVersion":                  func() upgradev1alpha1.PreflightCheckResult { return r.checkClusterVersion(ctx, logger, timeout) },
		"UpgradeInFlight":                 func() upgradev1alpha1.PreflightCheckResult { return r.checkUpgradeInFlight(ctx, logger, timeout) },
		"ClusterOperators":                func() upgradev1alpha1.PreflightCheckResult { return r.checkClusterOperators(ctx, logger, timeout) },
		"ClusterOperatorsExtended":        func() upgradev1alpha1.PreflightCheckResult { return r.checkClusterOperatorsExtended(ctx, logger, timeout) },
		"CriticalAlerts":                  func() upgradev1alpha1.PreflightCheckResult { return r.checkCriticalAlerts(ctx, logger, timeout) },
		"CriticalAlertsExtended":          func() upgradev1alpha1.PreflightCheckResult { return r.checkCriticalAlertsExtended(ctx, logger, timeout) },
		"PodDisruptionBudgets":                    func() upgradev1alpha1.PreflightCheckResult { return r.checkPodDisruptionBudgets(ctx, logger, timeout) },
		"PodDisruptionBudgetsMasterInfraExtended": func() upgradev1alpha1.PreflightCheckResult { return r.checkPodDisruptionBudgetsMasterInfraExtended(ctx, logger, timeout) },
		"ManuallyCordonedNodes":           func() upgradev1alpha1.PreflightCheckResult { return r.checkManuallyCordonedNodes(ctx, logger, timeout) },
		"ManuallyCordonedNodesExtended":   func() upgradev1alpha1.PreflightCheckResult { return r.checkManuallyCordonedNodesExtended(ctx, logger, timeout) },
		"NodeUnschedulableTaints":         func() upgradev1alpha1.PreflightCheckResult { return r.checkNodeTaints(ctx, logger, timeout) },
		"NodeUnschedulableTaintsExtended": func() upgradev1alpha1.PreflightCheckResult { return r.checkNodeTaintsExtended(ctx, logger, timeout) },
		"UpgradeableCondition":            func() upgradev1alpha1.PreflightCheckResult { return r.checkUpgradeableCondition(ctx, logger, timeout) },
		"PullSecretValid":                 func() upgradev1alpha1.PreflightCheckResult { return r.checkPullSecretValid(ctx, logger, timeout) },
		"AdminAcks":                       func() upgradev1alpha1.PreflightCheckResult { return r.checkAdminAcks(ctx, instance, logger, timeout) },
	}

	var results []upgradev1alpha1.PreflightCheckResult
	for _, name := range allChecks {
		if !checkAllowed(name, enabled) {
			results = append(results, upgradev1alpha1.PreflightCheckResult{
				Name:           name,
				Result:         upgradev1alpha1.CheckResultSkipped,
				TimeoutSeconds: int32(timeout.Seconds()),
				Execution:      upgradev1alpha1.CheckExecution{State: upgradev1alpha1.CheckSkipped},
			})
			continue
		}
		fn := dispatch[name]
		if fn == nil {
			results = append(results, errorResult(name, fmt.Sprintf("check %q has no registered implementation", name), timeout))
			continue
		}
		results = append(results, safeRun(name, fn, timeout))
	}
	return results
}

func checkTimeout(instance *upgradev1alpha1.UpgradePreflightCheck) time.Duration {
	if instance.Spec.CheckTimeoutSeconds > 0 {
		return time.Duration(instance.Spec.CheckTimeoutSeconds) * time.Second
	}
	return defaultCheckTimeout
}

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

func safeRun(name string, fn func() upgradev1alpha1.PreflightCheckResult, timeout time.Duration) (result upgradev1alpha1.PreflightCheckResult) {
	defer func() {
		if r := recover(); r != nil {
			result = upgradev1alpha1.PreflightCheckResult{
				Name:           name,
				Result:         upgradev1alpha1.CheckResultError,
				TimeoutSeconds: int32(timeout.Seconds()),
				Execution: upgradev1alpha1.CheckExecution{
					State: upgradev1alpha1.CheckError,
					Error: fmt.Sprintf("check panicked: %v", r),
				},
			}
		}
	}()
	return fn()
}

func completedResult(name string, result upgradev1alpha1.CheckResult, summary string, findings []string, timeout time.Duration) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name:           name,
		Result:         result,
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution: upgradev1alpha1.CheckExecution{
			State:    upgradev1alpha1.CheckCompleted,
			Summary:  summary,
			Findings: findings,
		},
	}
}

func errorResult(name, errMsg string, timeout time.Duration) upgradev1alpha1.PreflightCheckResult {
	return upgradev1alpha1.PreflightCheckResult{
		Name:           name,
		Result:         upgradev1alpha1.CheckResultError,
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution: upgradev1alpha1.CheckExecution{
			State: upgradev1alpha1.CheckError,
			Error: errMsg,
		},
	}
}

func withDesc(r upgradev1alpha1.PreflightCheckResult, desc string) upgradev1alpha1.PreflightCheckResult {
	r.Description = desc
	return r
}

func withTiming(r upgradev1alpha1.PreflightCheckResult, start, end metav1.Time) upgradev1alpha1.PreflightCheckResult {
	r.Execution.StartedAt = &start
	r.Execution.CompletedAt = &end
	return r
}

func withOwner(r upgradev1alpha1.PreflightCheckResult, owner upgradev1alpha1.CheckOwner) upgradev1alpha1.PreflightCheckResult {
	r.Owner = owner
	return r
}

func withAction(r upgradev1alpha1.PreflightCheckResult, action string) upgradev1alpha1.PreflightCheckResult {
	r.Action = action
	return r
}

func createFakeUpgradeConfig(targetVersion string) *upgradev1alpha1.UpgradeConfig {
	return &upgradev1alpha1.UpgradeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "preflight-check"},
		Spec:       upgradev1alpha1.UpgradeConfigSpec{Desired: upgradev1alpha1.Update{Version: targetVersion}},
		Status: upgradev1alpha1.UpgradeConfigStatus{
			History: upgradev1alpha1.UpgradeHistories{
				{Version: targetVersion, Phase: upgradev1alpha1.UpgradePhaseNew},
			},
		},
	}
}

func (r *ReconcileUpgradePreflightCheck) checkClusterVersion(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ClusterVersion"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to get cluster version: %v", err), timeout), descClusterVersion), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if ClusterVersion cannot be queried."), start, metav1.Now())
	}
	version := ""
	if clusterVersion != nil {
		version = clusterVersion.Status.Desired.Version
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		fmt.Sprintf("Cluster version %s", version),
		[]string{fmt.Sprintf("currentVersion=%s", version)},
		timeout), descClusterVersion), upgradev1alpha1.OwnerServiceTeam), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkClusterOperators(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ClusterOperators"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("failed to create metrics client: %v", err), timeout), descClusterOperators), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if cluster operator status cannot be determined."), start, metav1.Now())
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.ClusterOperators)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.ClusterOperators(metricsClient, r.CvClientBuilder.New(r.Client), fakeUC, logger, "")
	if err != nil || !passed {
		msg := "degraded operators detected"
		if err != nil {
			msg = err.Error()
		}
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("Upgrade validation failed: %s", msg), findings, timeout), descClusterOperators), upgradev1alpha1.OwnerCustomer), "Investigate degraded cluster operators: `oc get co`. Check pod logs in the affected operator namespace."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Upgrade validation passed: no degraded operators detected", findings, timeout), descClusterOperators), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkClusterOperatorsExtended(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ClusterOperatorsExtended"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var findings []string
	coList := &configv1.ClusterOperatorList{}
	if err := r.Client.List(checkCtx, coList); err == nil {
		for _, co := range coList.Items {
			availStatus, degradedStatus := "Unknown", "Unknown"
			for _, cond := range co.Status.Conditions {
				if cond.Type == configv1.OperatorAvailable {
					availStatus = string(cond.Status)
				}
				if cond.Type == configv1.OperatorDegraded {
					degradedStatus = string(cond.Status)
				}
			}
			findings = append(findings, fmt.Sprintf("%s: Available=%s, Degraded=%s", co.Name, availStatus, degradedStatus))
		}
	}

	cvClient := r.CvClientBuilder.New(r.Client)
	degraded, err := cvClient.HasDegradedOperators()
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to fetch cluster operator status: %v", err), timeout), descClusterOperatorsExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if cluster operator status cannot be listed."), start, metav1.Now())
	}
	if len(degraded.Degraded) > 0 {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("%d degraded operator(s): %s", len(degraded.Degraded), strings.Join(degraded.Degraded, ", ")),
			degraded.Degraded, timeout), descClusterOperatorsExtended), upgradev1alpha1.OwnerCustomer), "Investigate the listed degraded operators. Check pod logs and events in their namespaces."), start, metav1.Now())
	}
	summary := "No degraded operators detected"
	if len(coList.Items) > 0 {
		summary = fmt.Sprintf("All %d cluster operators healthy", len(coList.Items))
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed, summary, findings, timeout), descClusterOperatorsExtended), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkCriticalAlerts(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "CriticalAlerts"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("failed to create metrics client: %v", err), timeout), descCriticalAlerts), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if Prometheus metrics cannot be queried."), start, metav1.Now())
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.CriticalAlertsDefault)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.CriticalAlertsDefault(metricsClient, fakeUC, logger, "")
	if err != nil || !passed {
		msg := "critical alerts firing"
		if err != nil {
			msg = err.Error()
		}
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("Upgrade validation failed: %s", msg), findings, timeout), descCriticalAlerts), upgradev1alpha1.OwnerCustomer), "Resolve the firing critical alerts before attempting an upgrade."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Upgrade validation passed: no critical alerts firing", findings, timeout), descCriticalAlerts), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkCriticalAlertsExtended(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "CriticalAlertsExtended"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var alerts *metrics.AlertResponse
	var err error
	query := `ALERTS{alertstate="firing",severity="critical",namespace=~"^openshift.*|^kube-.*|^default$"}`

	for attempt := 1; attempt <= prometheusRetries; attempt++ {
		var metricsClient metrics.Metrics
		metricsClient, err = r.MetricsClientBuilder.NewClient(r.Client)
		if err != nil {
			if attempt < prometheusRetries {
				select {
				case <-checkCtx.Done():
					return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("timed out waiting for metrics client: %v", err), timeout), descCriticalAlertsExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if Prometheus cannot be reached."), start, metav1.Now())
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		alerts, err = metricsClient.Query(query)
		if err != nil {
			if attempt < prometheusRetries {
				select {
				case <-checkCtx.Done():
					return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("timed out querying Prometheus: %v", err), timeout), descCriticalAlertsExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if Prometheus cannot be reached."), start, metav1.Now())
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		break
	}
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to query critical alerts after %d attempts: %v", prometheusRetries, err), timeout), descCriticalAlertsExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if Prometheus metrics cannot be queried."), start, metav1.Now())
	}
	if len(alerts.Data.Result) == 0 {
		return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
			"No critical alerts firing in core namespaces",
			[]string{"No firing critical alerts in openshift-*, kube-*, or default namespaces"},
			timeout), descCriticalAlertsExtended), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
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
	return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
		fmt.Sprintf("%d critical alert(s) firing: %s", len(names), strings.Join(names, ", ")),
		names, timeout), descCriticalAlertsExtended), upgradev1alpha1.OwnerCustomer), "Investigate and resolve the listed critical alerts before upgrading."), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkManuallyCordonedNodes(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ManuallyCordonedNodes"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("failed to create metrics client: %v", err), timeout), descNodeHealth), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node status cannot be determined."), start, metav1.Now())
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.ManuallyCordonedNodes)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	cordonedNodes, err := upgraders.ManuallyCordonedNodes(metricsClient, r.Machinery, r.Client, fakeUC, logger, "")
	if err != nil && len(cordonedNodes) == 0 {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to check cordoned nodes: %v", err), timeout), descNodeHealth), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node status cannot be determined."), start, metav1.Now())
	}
	if len(cordonedNodes) > 0 {
		for _, node := range cordonedNodes {
			findings = append(findings, fmt.Sprintf("cordoned worker node: %s", node))
		}
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("Upgrade validation failed: %d manually cordoned worker node(s)", len(cordonedNodes)),
			findings, timeout), descNodeHealth), upgradev1alpha1.OwnerCustomer), "Uncordon the listed nodes before upgrading: `oc adm uncordon <node-name>`"), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Upgrade validation passed: no manually cordoned nodes", findings, timeout), descNodeHealth), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkManuallyCordonedNodesExtended(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "ManuallyCordonedNodesExtended"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes, &client.ListOptions{
		Raw: &metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker, !node-role.kubernetes.io/infra"},
	}); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list worker nodes: %v", err), timeout), descNodeHealthExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node status cannot be listed."), start, metav1.Now())
	}

	var cordoned, findings []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		cr := r.Machinery.IsNodeCordoned(node)
		if cr.IsCordoned && !r.Machinery.IsNodeUpgrading(node) {
			cordoned = append(cordoned, node.Name)
			findings = append(findings, fmt.Sprintf("%s: manually cordoned", node.Name))
		} else {
			findings = append(findings, fmt.Sprintf("%s: schedulable", node.Name))
		}
	}
	if len(cordoned) > 0 {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("%d manually cordoned node(s) of %d worker node(s)", len(cordoned), len(nodes.Items)),
			findings, timeout), descNodeHealthExtended), upgradev1alpha1.OwnerCustomer), "Uncordon the listed nodes before upgrading: `oc adm uncordon <node-name>`"), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		fmt.Sprintf("All %d worker node(s) are schedulable", len(nodes.Items)),
		findings, timeout), descNodeHealthExtended), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkNodeTaints(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "NodeUnschedulableTaints"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("failed to create metrics client: %v", err), timeout), descNodeHealth), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node taint status cannot be determined."), start, metav1.Now())
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.NodeUnschedulableTaints)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	taintedNodes, err := upgraders.NodeUnschedulableTaints(metricsClient, r.Machinery, r.Client, fakeUC, logger, "")
	if err != nil && len(taintedNodes) == 0 {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to check node taints: %v", err), timeout), descNodeHealth), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node taint status cannot be determined."), start, metav1.Now())
	}
	if len(taintedNodes) > 0 {
		for _, node := range taintedNodes {
			findings = append(findings, fmt.Sprintf("node with pressure taint: %s", node))
		}
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("Upgrade validation failed: %d node(s) with pressure taints", len(taintedNodes)),
			findings, timeout), descNodeHealth), upgradev1alpha1.OwnerCustomer), "Remove or adjust the unschedulable taints on the listed nodes before upgrading."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Upgrade validation passed: no pressure taints detected", findings, timeout), descNodeHealth), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkNodeTaintsExtended(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "NodeUnschedulableTaintsExtended"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodes := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodes); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list nodes: %v", err), timeout), descNodeHealthExtended), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node taint status cannot be listed."), start, metav1.Now())
	}
	var affected []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if r.Machinery.HasMemoryPressure(node) || r.Machinery.HasDiskPressure(node) || r.Machinery.HasPidPressure(node) {
			affected = append(affected, node.Name)
		}
	}
	if len(affected) > 0 {
		findings := []string{fmt.Sprintf("Total nodes checked: %d", len(nodes.Items))}
		for _, name := range affected {
			findings = append(findings, fmt.Sprintf("%s: unschedulable pressure taint", name))
		}
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("%d node(s) with pressure taints of %d total", len(affected), len(nodes.Items)),
			findings, timeout), descNodeHealthExtended), upgradev1alpha1.OwnerCustomer), "Remove or adjust the unschedulable taints on the listed nodes before upgrading."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		fmt.Sprintf("All %d node(s) checked: no pressure taints", len(nodes.Items)),
		[]string{fmt.Sprintf("Checked %d node(s) for memory, disk, and PID pressure — all healthy", len(nodes.Items))},
		timeout), descNodeHealthExtended), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkPodDisruptionBudgets(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "PodDisruptionBudgets"
	start := metav1.Now()

	if r.DvoClientBuilder == nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, "DvoClientBuilder is not configured", timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support — the DVO client is missing from the operator configuration."), start, metav1.Now())
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if checkCtx.Err() != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("context done before check could start: %v", checkCtx.Err()), timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if the PDB check consistently times out."), start, metav1.Now())
	}
	metricsClient, err := r.MetricsClientBuilder.NewClient(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to build metrics client: %v", err), timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if Prometheus cannot be reached."), start, metav1.Now())
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.HealthCheckPDB)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	_, passed, err := upgraders.HealthCheckPDB(metricsClient, r.Client, r.DvoClientBuilder, fakeUC, logger, fakeUC.Spec.Desired.Version)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("PodDisruptionBudget check failed: %v", err), timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if the PDB check fails to execute."), start, metav1.Now())
	}
	if !passed {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			"Upgrade validation failed: blocking PDBs or invalid PDB configuration detected",
			findings, timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerCustomer), "Adjust PodDisruptionBudgets in openshift-* namespaces to allow at least one pod to be unavailable during node drains (MaxUnavailable≥1 or MinAvailable<100%)."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Upgrade validation passed: no blocking PDBs", findings, timeout), descPodDisruptionBudgets), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

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

func (r *ReconcileUpgradePreflightCheck) checkPodDisruptionBudgetsMasterInfraExtended(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "PodDisruptionBudgetsMasterInfraExtended"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nodeList := &corev1.NodeList{}
	if err := r.Client.List(checkCtx, nodeList); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list nodes: %v", err), timeout), descPDBMasterInfra), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if node listing fails."), start, metav1.Now())
	}

	masterInfraNames := map[string]string{}
	for _, node := range nodeList.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			masterInfraNames[node.Name] = "master"
		} else if _, ok := node.Labels["node-role.kubernetes.io/infra"]; ok {
			masterInfraNames[node.Name] = "infra"
		}
	}

	// Source 1: pods currently running on master/infra nodes.
	podList := &corev1.PodList{}
	if err := r.Client.List(checkCtx, podList); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list pods: %v", err), timeout), descPDBMasterInfra), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if pod listing fails."), start, metav1.Now())
	}
	masterInfraPods := []corev1.Pod{}
	for _, pod := range podList.Items {
		if _, ok := masterInfraNames[pod.Spec.NodeName]; ok {
			masterInfraPods = append(masterInfraPods, pod)
		}
	}

	// Source 2: DaemonSet pod templates that tolerate master/infra taints.
	// DaemonSets with these tolerations will always place pods on master/infra
	// nodes regardless of whether pods are currently running.
	dsList := &appsv1.DaemonSetList{}
	if err := r.Client.List(checkCtx, dsList); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list DaemonSets: %v", err), timeout), descPDBMasterInfra), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if DaemonSet listing fails."), start, metav1.Now())
	}

	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := r.Client.List(checkCtx, pdbList); err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to list PodDisruptionBudgets: %v", err), timeout), descPDBMasterInfra), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if PDB listing fails."), start, metav1.Now())
	}

	// Track PDBs already flagged to avoid duplicate findings.
	flagged := map[string]bool{}
	var blocking, findings []string

	for _, pdb := range pdbList.Items {
		key := fmt.Sprintf("%s/%s", pdb.Namespace, pdb.Name)

		// Check running pods on master/infra nodes.
		if covered := pdbCoveredMasterInfraPods(pdb, masterInfraPods, masterInfraNames); len(covered) > 0 {
			flagged[key] = true
			if reason := pdbViolation(pdb); reason != "" {
				detail := fmt.Sprintf("%s: covers %d running pods on master/infra nodes (%s)", key, len(covered), reason)
				blocking = append(blocking, detail)
				findings = append(findings, detail)
			} else {
				findings = append(findings, fmt.Sprintf("%s: ok (%d running pods on master/infra)", key, len(covered)))
			}
			continue
		}

		// Check DaemonSets that target master/infra nodes.
		if ds, matched := pdbMatchesMasterInfraDS(pdb, dsList.Items); matched {
			flagged[key] = true
			if reason := pdbViolation(pdb); reason != "" {
				detail := fmt.Sprintf("%s: covers DaemonSet %s/%s targeting master/infra nodes (%s)", key, ds.Namespace, ds.Name, reason)
				blocking = append(blocking, detail)
				findings = append(findings, detail)
			} else {
				findings = append(findings, fmt.Sprintf("%s: ok (covers DaemonSet %s/%s targeting master/infra)", key, ds.Namespace, ds.Name))
			}
		}
	}

	if len(blocking) > 0 {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			fmt.Sprintf("Found %d restrictive PDB(s) that could block drain of %d master/infra node(s)",
				len(blocking), len(masterInfraNames)),
			findings, timeout), descPDBMasterInfra), upgradev1alpha1.OwnerCustomer), "Adjust the listed PodDisruptionBudgets to allow node drains, or move the workloads off master/infra nodes."), start, metav1.Now())
	}
	summary := fmt.Sprintf("Checked PDBs against %d master/infra node(s): none blocking", len(masterInfraNames))
	if len(masterInfraNames) == 0 {
		summary = "No master/infra nodes found"
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed, summary, findings, timeout), descPDBMasterInfra), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func pdbCoveredMasterInfraPods(pdb policyv1.PodDisruptionBudget, pods []corev1.Pod, nodeRoles map[string]string) []corev1.Pod {
	if pdb.Spec.Selector == nil {
		return nil
	}
	var covered []corev1.Pod
	for _, pod := range pods {
		if pod.Namespace != pdb.Namespace {
			continue
		}
		if _, onMasterInfra := nodeRoles[pod.Spec.NodeName]; !onMasterInfra {
			continue
		}
		if podMatchesPDBSelector(pod, pdb) {
			covered = append(covered, pod)
		}
	}
	return covered
}

func podMatchesPDBSelector(pod corev1.Pod, pdb policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	for k, v := range pdb.Spec.Selector.MatchLabels {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

// pdbMatchesMasterInfraDS returns the first DaemonSet whose pod template is covered
// by the PDB's selector and whose pod template tolerates master or infra taints.
func pdbMatchesMasterInfraDS(pdb policyv1.PodDisruptionBudget, dsList []appsv1.DaemonSet) (appsv1.DaemonSet, bool) {
	if pdb.Spec.Selector == nil {
		return appsv1.DaemonSet{}, false
	}
	for _, ds := range dsList {
		if ds.Namespace != pdb.Namespace {
			continue
		}
		if !dsToleratsMasterOrInfra(ds) {
			continue
		}
		if dsMatchesPDBSelector(ds, pdb) {
			return ds, true
		}
	}
	return appsv1.DaemonSet{}, false
}

func dsToleratsMasterOrInfra(ds appsv1.DaemonSet) bool {
	for _, t := range ds.Spec.Template.Spec.Tolerations {
		if t.Key == "node-role.kubernetes.io/master" || t.Key == "node-role.kubernetes.io/infra" {
			return true
		}
		// A wildcard toleration (key="", operator=Exists) covers all taints including master/infra.
		if t.Key == "" && t.Operator == corev1.TolerationOpExists {
			return true
		}
	}
	return false
}

func dsMatchesPDBSelector(ds appsv1.DaemonSet, pdb policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	podLabels := ds.Spec.Template.Labels
	for k, v := range pdb.Spec.Selector.MatchLabels {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

func (r *ReconcileUpgradePreflightCheck) checkUpgradeableCondition(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "UpgradeableCondition"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to get cluster version: %v", err), timeout), descUpgradeableCondition), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if ClusterVersion cannot be queried."), start, metav1.Now())
	}
	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorUpgradeable && condition.Status == configv1.ConditionFalse {
			return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
				fmt.Sprintf("Cluster not upgradeable: %s", condition.Reason),
				[]string{
					fmt.Sprintf("Upgradeable=False: reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				}, timeout), descUpgradeableCondition), upgradev1alpha1.OwnerCustomer), "Resolve the Upgradeable=False condition. For AdminAck gates, use the ROSA CLI or update the admin-acks ConfigMap."), start, metav1.Now())
		}
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Cluster is upgradeable",
		[]string{"Upgradeable condition is True (or not set)"},
		timeout), descUpgradeableCondition), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkPullSecretValid(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "PullSecretValid"
	start := metav1.Now()

	_, err := util.GetAccessToken(r.Client)
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			"Pull secret validation failed",
			[]string{err.Error()},
			timeout), descPullSecretValid), upgradev1alpha1.OwnerCustomer), "Renew the cluster pull secret at cloud.openshift.com and update openshift-config/pull-secret."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"Pull secret is valid",
		[]string{"cloud.openshift.com auth entry is present in the cluster pull secret"},
		timeout), descPullSecretValid), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkAdminAcks(
	ctx context.Context,
	instance *upgradev1alpha1.UpgradePreflightCheck,
	logger logr.Logger,
	timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "AdminAcks"
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targetVersion := instance.Spec.Desired.Version
	isYStream := targetVersion != ""

	cm := &corev1.ConfigMap{}
	err := r.Client.Get(checkCtx, types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isYStream {
				return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
					"admin-acks ConfigMap not found — required for y-stream upgrades",
					[]string{"ConfigMap openshift-config/admin-acks does not exist"},
					timeout), descAdminAcks), upgradev1alpha1.OwnerCustomer), "Acknowledge the required upgrade gates. For ROSA, use `rosa upgrade cluster -c <cluster-id>`."), start, metav1.Now())
			}
			return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
				"Admin acks not required for z-stream upgrade",
				[]string{"ConfigMap openshift-config/admin-acks does not exist (not required for z-stream)"},
				timeout), descAdminAcks), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
		}
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to read admin-acks ConfigMap: %v", err), timeout), descAdminAcks), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if the admin-acks ConfigMap cannot be read."), start, metav1.Now())
	}

	var ackKeys []string
	for k := range cm.Data {
		ackKeys = append(ackKeys, fmt.Sprintf("%s=%s", k, cm.Data[k]))
	}
	if isYStream && len(cm.Data) == 0 {
		return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
			"admin-acks ConfigMap exists but has no entries",
			[]string{"ConfigMap openshift-config/admin-acks has no data entries"},
			timeout), descAdminAcks), upgradev1alpha1.OwnerCustomer), "Acknowledge the required upgrade gates. For ROSA, use `rosa upgrade cluster -c <cluster-id>`."), start, metav1.Now())
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		fmt.Sprintf("Admin acks present: %d entry/entries in openshift-config/admin-acks", len(ackKeys)),
		ackKeys, timeout), descAdminAcks), upgradev1alpha1.OwnerCustomer), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) checkUpgradeInFlight(
	ctx context.Context, logger logr.Logger, timeout time.Duration,
) upgradev1alpha1.PreflightCheckResult {
	const checkName = "UpgradeInFlight"
	start := metav1.Now()
	_, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cvClient := r.CvClientBuilder.New(r.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return withTiming(withAction(withOwner(withDesc(errorResult(checkName, fmt.Sprintf("unable to get cluster version: %v", err), timeout), descUpgradeInFlight), upgradev1alpha1.OwnerServiceTeam), "Contact Red Hat support if ClusterVersion cannot be queried."), start, metav1.Now())
	}
	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorProgressing && condition.Status == configv1.ConditionTrue {
			return withTiming(withAction(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckFailed,
				fmt.Sprintf("Upgrade already in progress: %s", condition.Message),
				[]string{
					fmt.Sprintf("Progressing=True: reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				}, timeout), descUpgradeInFlight), upgradev1alpha1.OwnerServiceTeam), "Wait for the in-progress upgrade to complete before starting another. Contact Red Hat support if the upgrade appears stuck."), start, metav1.Now())
		}
	}
	return withTiming(withOwner(withDesc(completedResult(checkName, upgradev1alpha1.CheckPassed,
		"No upgrade in progress",
		[]string{"ClusterVersion Progressing condition is False (or not set)"},
		timeout), descUpgradeInFlight), upgradev1alpha1.OwnerServiceTeam), start, metav1.Now())
}

func (r *ReconcileUpgradePreflightCheck) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.UpgradePreflightCheck{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
