package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/dvo"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	"github.com/openshift/managed-upgrade-operator/pkg/ocm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var log = logf.Log.WithName("controller_upgradediagscheck")

const (
	maxDiagsChecks    = 10
	diagsCheckTTL     = 24 * time.Hour
	deleteAfterAnnotation = "upgradediags.managed.openshift.io/delete-after"
)

// remainingOCMGaps lists OCM-side checks that still require work beyond what
// ocmLimitedSupportCheck, ocmUpgradePolicyCheck, and ocmVersionGateCheck already cover.
//
//   - NO_TARGETS_AVAILABLE / AT_LATEST_VERSION / TARGET_NOT_AVAILABLE: OCM/ROSA upgrade
//     graph visibility. OCM owns the list of ROSA-approved versions and may silently omit
//     a version that CVO and Cincinnati consider valid, with no structured reason exposed.
//     The UpgradeTargetExtended check surfaces an advisory pointing admins to OCMVersionGates
//     and OCMUpgradePolicy as the two most common causes, but a definitive check would
//     require the OCM upgrade graph API to expose a blocked_reason or availability_state
//     field. MCS-1506 tracks the upstream OCM API work.
var _ = "remainingOCMGaps — see comment above"

// ReconcileUpgradeDiagsCheck reconciles UpgradeDiagsCheck objects.
type ReconcileUpgradeDiagsCheck struct {
	Client               client.Client
	Scheme               *runtime.Scheme
	MetricsClientBuilder metrics.MetricsBuilder
	CvClientBuilder      cv.ClusterVersionBuilder
	Machinery            machinery.Machinery
	DvoClientBuilder     dvo.DvoClientBuilder
	OcmClientBuilder     ocm.OcmClientBuilder
}

func (r *ReconcileUpgradeDiagsCheck) pruneAndCount(ctx context.Context, namespace, skipName string, logger logr.Logger) (int, error) {
	list := &upgradev1alpha1.UpgradeDiagsCheckList{}
	if err := r.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return 0, fmt.Errorf("listing UpgradeDiagsChecks: %w", err)
	}
	now := time.Now()
	remaining := 0
	for i := range list.Items {
		cr := &list.Items[i]
		expired := cr.CreationTimestamp.Time.Add(diagsCheckTTL).Before(now)
		if ann := cr.Annotations[deleteAfterAnnotation]; ann != "" {
			if t, err := time.Parse(time.RFC3339, ann); err == nil && t.Before(now) {
				expired = true
			}
		}
		if expired && cr.Name != skipName {
			logger.Info("Pruning expired UpgradeDiagsCheck", "name", cr.Name)
			if err := r.Client.Delete(ctx, cr); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "Failed to prune expired UpgradeDiagsCheck", "name", cr.Name)
			}
			continue
		}
		remaining++
	}
	return remaining, nil
}

func (r *ReconcileUpgradeDiagsCheck) Reconcile(ctx context.Context, req reconcile.Request) (result reconcile.Result, err error) {
	logger := log.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)

	defer func() {
		if p := recover(); p != nil {
			logger.Error(fmt.Errorf("%v", p), "panic recovered in readiness reconcile")
			result, err = reconcile.Result{}, nil
		}
	}()

	logger.Info("Reconciling UpgradeDiagsCheck")

	instance := &upgradev1alpha1.UpgradeDiagsCheck{}
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
		logger.Error(listErr, "Could not list UpgradeDiagsChecks; skipping limit enforcement")
	} else if total >= maxDiagsChecks {
		now := metav1.NewTime(time.Now())
		if instance.Status.StartedAt == nil {
			instance.Status.StartedAt = &now
		}
		instance.Status.CompletedAt = &now
		instance.Status.Checks = []upgradev1alpha1.DiagsCheckResult{{
			Name:           "LimitCheck",
			Result:         upgradev1alpha1.CheckResultError,
			TimeoutSeconds: 0,
			Execution: upgradev1alpha1.CheckExecution{
				State: upgradev1alpha1.CheckError,
				Error: fmt.Sprintf("too many UpgradeDiagsCheck CRs (%d/%d). Delete completed checks before creating new ones.", total, maxDiagsChecks),
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
		// Write StartedAt first so the reconcile can always make forward progress,
		// even if the CRD schema predates the cluster-identifier fields.
		if err := r.Client.Status().Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
		// Best-effort: populate cluster identifier. If the CRD schema does not yet
		// include clusterInfraName/apiServerURL the status write will be rejected
		// with "unknown field"; in that case clear the fields and continue — checks
		// will still run and the HTTP handler will fetch the identifier directly.
		infra := &unstructured.Unstructured{}
		infra.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure",
		})
		if err := r.Client.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
			logger.Error(err, "Failed to read Infrastructure CR — cluster identifier will be omitted from status")
		} else {
			instance.Status.ClusterInfraName, _, _ = unstructured.NestedString(infra.Object, "status", "infrastructureName")
			instance.Status.APIServerURL, _, _ = unstructured.NestedString(infra.Object, "status", "apiServerURL")
			if err := r.Client.Status().Update(ctx, instance); err != nil {
				logger.Info("Cluster identifier fields not yet in CRD schema — omitting from status until CRD is updated", "err", err)
				instance.Status.ClusterInfraName = ""
				instance.Status.APIServerURL = ""
			}
		}
	}

	checks := r.runChecks(ctx, instance, logger)

	now := metav1.NewTime(time.Now())
	instance.Status.CompletedAt = &now
	instance.Status.Checks = checks
	applyAggregates(&instance.Status, checks, now)
	if err := r.Client.Status().Update(ctx, instance); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("UpgradeDiagsCheck completed", "totalChecks", len(checks))
	return reconcile.Result{}, nil
}

// applyAggregates computes overall result counts and sets the Complete condition.
func applyAggregates(status *upgradev1alpha1.UpgradeDiagsCheckStatus, checks []upgradev1alpha1.DiagsCheckResult, now metav1.Time) {
	var passed, warned, failed, errored, skipped int
	for _, c := range checks {
		switch c.Result {
		case upgradev1alpha1.CheckPassed:
			passed++
		case upgradev1alpha1.CheckResultWarning:
			warned++
		case upgradev1alpha1.CheckFailed:
			failed++
		case upgradev1alpha1.CheckResultError:
			errored++
		case upgradev1alpha1.CheckResultSkipped:
			skipped++
		}
	}

	var overall upgradev1alpha1.CheckResult
	switch {
	case failed > 0 || errored > 0:
		overall = upgradev1alpha1.CheckFailed
	case warned > 0:
		overall = upgradev1alpha1.CheckResultWarning
	default:
		overall = upgradev1alpha1.CheckPassed
	}

	status.OverallResult = overall
	status.ChecksPassed = passed
	status.ChecksWarned = warned
	status.ChecksFailed = failed
	status.ChecksErrored = errored
	status.ChecksSkipped = skipped

	msg := fmt.Sprintf("%d passed, %d warned, %d failed, %d errored, %d skipped", passed, warned, failed, errored, skipped)
	status.Conditions = []metav1.Condition{{
		Type:               upgradev1alpha1.DiagsCompleteCondition,
		Status:             metav1.ConditionTrue,
		Reason:             string(overall),
		Message:            msg,
		LastTransitionTime: now,
	}}
}

func (r *ReconcileUpgradeDiagsCheck) runChecks(
	ctx context.Context,
	instance *upgradev1alpha1.UpgradeDiagsCheck,
	logger logr.Logger,
) []upgradev1alpha1.DiagsCheckResult {
	deps := CheckDependencies{
		Client:               r.Client,
		MetricsClientBuilder: r.MetricsClientBuilder,
		CvClientBuilder:      r.CvClientBuilder,
		Machinery:            r.Machinery,
		DvoClientBuilder:     r.DvoClientBuilder,
		OcmClientBuilder:     r.OcmClientBuilder,
		DesiredVersion:       instance.Spec.Desired.Version,
		DesiredChannel:       instance.Spec.Desired.Channel,
	}
	enabled := instance.Spec.Checks

	runCtx := ctx
	if instance.Spec.OverallTimeoutSeconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(instance.Spec.OverallTimeoutSeconds)*time.Second)
		defer cancel()
	}

	var results []upgradev1alpha1.DiagsCheckResult
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

func (r *ReconcileUpgradeDiagsCheck) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.UpgradeDiagsCheck{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 2}).
		Complete(r)
}
