package upgradediags

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const descClusterAutoscalerConfig = "Validates the ClusterAutoscaler 'cluster' object configuration. An invalid utilizationThreshold or maxNodesTotal causes the autoscaler operator to degrade, which then blocks the upgrade health gate. Source: OHSS Case 5 (OHSS-56299: invalid ClusterAutoscaler config delaying 4.19)."

var clusterAutoscalerGVK = schema.GroupVersionKind{Group: "autoscaling.openshift.io", Version: "v1", Kind: "ClusterAutoscaler"}

type clusterAutoscalerConfigCheck struct{}

func (c *clusterAutoscalerConfigCheck) Name() string                 { return "ClusterAutoscalerConfig" }
func (c *clusterAutoscalerConfigCheck) Description() string           { return descClusterAutoscalerConfig }
func (c *clusterAutoscalerConfigCheck) DefaultTimeout() time.Duration { return 0 }
func (c *clusterAutoscalerConfigCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *clusterAutoscalerConfigCheck) UpgradePhase() string { return "worker-upgrade" }

func (c *clusterAutoscalerConfigCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	ca := &unstructured.Unstructured{}
	ca.SetGroupVersionKind(clusterAutoscalerGVK)
	err := deps.Client.Get(ctx, types.NamespacedName{Name: "default"}, ca)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Try the name "cluster" as well
			ca2 := &unstructured.Unstructured{}
			ca2.SetGroupVersionKind(clusterAutoscalerGVK)
			err2 := deps.Client.Get(ctx, types.NamespacedName{Name: "cluster"}, ca2)
			if apierrors.IsNotFound(err2) {
				return CheckOutput{
					Result:   upgradev1alpha1.CheckPassed,
					Summary:  "No ClusterAutoscaler configured — skipped",
					Findings: []string{"ClusterAutoscaler 'default' and 'cluster' objects not found; autoscaler is not in use"},
				}
			}
			if err2 != nil {
				return autoscalerGetError(err2)
			}
			ca = ca2
		} else {
			return autoscalerGetError(err)
		}
	}

	var problems []string
	var findings []string

	// Validate spec.scaleDown.utilizationThreshold (must be a float in (0.0, 1.0])
	ut, found, _ := unstructured.NestedString(ca.Object, "spec", "scaleDown", "utilizationThreshold")
	if found && ut != "" {
		f, err := strconv.ParseFloat(ut, 64)
		if err != nil {
			problems = append(problems, fmt.Sprintf("spec.scaleDown.utilizationThreshold=%q is not a valid float: %v", ut, err))
		} else if f <= 0.0 || f > 1.0 {
			problems = append(problems, fmt.Sprintf("spec.scaleDown.utilizationThreshold=%q is out of range (must be in (0.0, 1.0])", ut))
		} else {
			findings = append(findings, fmt.Sprintf("spec.scaleDown.utilizationThreshold=%q (valid)", ut))
		}
	}

	// Validate spec.resourceLimits.maxNodesTotal (must be > 0 when set)
	maxNodes, found, _ := unstructured.NestedInt64(ca.Object, "spec", "resourceLimits", "maxNodesTotal")
	if found {
		if maxNodes <= 0 {
			problems = append(problems, fmt.Sprintf("spec.resourceLimits.maxNodesTotal=%d is not positive (must be > 0)", maxNodes))
		} else {
			findings = append(findings, fmt.Sprintf("spec.resourceLimits.maxNodesTotal=%d (valid)", maxNodes))
		}
	}

	if len(problems) == 0 && len(findings) == 0 {
		findings = append(findings, "ClusterAutoscaler exists with default configuration — no validated fields found")
	}

	if len(problems) > 0 {
		findings = append(problems, findings...)
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d ClusterAutoscaler configuration problem(s) found", len(problems)),
			Findings: findings,
			Action:   "Fix the invalid ClusterAutoscaler field(s) listed above and wait for the autoscaler operator to recover before rescheduling the upgrade.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "ClusterAutoscaler configuration is valid",
		Findings: findings,
	}
}

func autoscalerGetError(err error) CheckOutput {
	if apierrors.IsForbidden(err) {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultWarning,
			Summary:  "RBAC not granted — cannot query ClusterAutoscaler",
			Findings: []string{fmt.Sprintf("forbidden: %v", err)},
			Owner:    upgradev1alpha1.OwnerServiceTeam,
			Action:   "Add 'clusterautoscalers' get permission for 'autoscaling.openshift.io' to the managed-upgrade-operator ClusterRole.",
		}
	}
	return CheckOutput{
		Result: upgradev1alpha1.CheckResultError,
		Error:  fmt.Sprintf("unable to get ClusterAutoscaler: %v", err),
		Owner:  upgradev1alpha1.OwnerServiceTeam,
		Action: "Contact Red Hat support if ClusterAutoscaler cannot be queried.",
	}
}
