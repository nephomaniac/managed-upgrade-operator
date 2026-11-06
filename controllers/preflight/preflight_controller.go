package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	muocfg "github.com/openshift/managed-upgrade-operator/config"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/configmanager"
	"github.com/openshift/managed-upgrade-operator/pkg/dvo"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var log = logf.Log.WithName("controller_upgradepreflightcheck")

const (
	preflightControllerFeatureGate = "PreflightController"
	maxPreflightChecks    = 10
	preflightCheckTTL     = 24 * time.Hour
	deleteAfterAnnotation = "preflight.managed.openshift.io/delete-after"
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
	deps := CheckDependencies{
		Client:               r.Client,
		MetricsClientBuilder: r.MetricsClientBuilder,
		CvClientBuilder:      r.CvClientBuilder,
		Machinery:            r.Machinery,
		DvoClientBuilder:     r.DvoClientBuilder,
		DesiredVersion:       instance.Spec.Desired.Version,
	}
	enabled := instance.Spec.Checks

	runCtx := ctx
	if instance.Spec.OverallTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(instance.Spec.OverallTimeoutSeconds)*time.Second)
		defer cancel()
	}

	var results []upgradev1alpha1.PreflightCheckResult
	for _, check := range registeredChecks {
		timeout := resolveCheckTimeout(check, instance.Spec)
		if runCtx.Err() != nil {
			results = append(results, timedOutResult(check, timeout))
			continue
		}
		if !checkAllowed(check.Name(), enabled) {
			results = append(results, skippedResult(check, timeout))
			continue
		}
		results = append(results, runCheck(check, runCtx, deps, logger, timeout))
	}
	return results
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

func (r *ReconcileUpgradePreflightCheck) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.UpgradePreflightCheck{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
