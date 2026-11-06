package preflight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
)

const (
	descClusterOperators         = "Validates that no cluster operators are degraded, using the same check MUO runs before starting an upgrade. Checks operator status via Prometheus metrics."
	descClusterOperatorsExtended = "Comprehensive cluster operator health check. Lists all operators with available/degraded conditions and detailed status messages."
)

type clusterOperatorsCheck struct{}

func (c *clusterOperatorsCheck) Name() string                 { return "ClusterOperators" }
func (c *clusterOperatorsCheck) Description() string           { return descClusterOperators }
func (c *clusterOperatorsCheck) DefaultTimeout() time.Duration { return 0 }
func (c *clusterOperatorsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *clusterOperatorsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	metricsClient, err := deps.MetricsClientBuilder.NewClient(deps.Client)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to create metrics client: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if cluster operator status cannot be determined.",
		}
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.ClusterOperators)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.ClusterOperators(metricsClient, deps.CvClientBuilder.New(deps.Client), fakeUC, logger, "")
	if err != nil || !passed {
		msg := "degraded operators detected"
		if err != nil {
			msg = err.Error()
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Upgrade validation failed: %s", msg),
			Findings: findings,
			Action:   "Investigate degraded cluster operators: `oc get co`. Check pod logs in the affected operator namespace.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Upgrade validation passed: no degraded operators detected",
		Findings: findings,
	}
}

type clusterOperatorsExtendedCheck struct{}

func (c *clusterOperatorsExtendedCheck) Name() string                 { return "ClusterOperatorsExtended" }
func (c *clusterOperatorsExtendedCheck) Description() string           { return descClusterOperatorsExtended }
func (c *clusterOperatorsExtendedCheck) DefaultTimeout() time.Duration { return 0 }
func (c *clusterOperatorsExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *clusterOperatorsExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	var findings []string
	coList := &configv1.ClusterOperatorList{}
	if err := deps.Client.List(ctx, coList); err == nil {
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

	cvClient := deps.CvClientBuilder.New(deps.Client)
	degraded, err := cvClient.HasDegradedOperators()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to fetch cluster operator status: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if cluster operator status cannot be listed.",
		}
	}
	if len(degraded.Degraded) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d degraded operator(s): %s", len(degraded.Degraded), strings.Join(degraded.Degraded, ", ")),
			Findings: degraded.Degraded,
			Action:   "Investigate the listed degraded operators. Check pod logs and events in their namespaces.",
		}
	}
	summary := "No degraded operators detected"
	if len(coList.Items) > 0 {
		summary = fmt.Sprintf("All %d cluster operators healthy", len(coList.Items))
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  summary,
		Findings: findings,
	}
}
