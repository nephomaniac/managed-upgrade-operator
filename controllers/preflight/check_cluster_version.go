package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
)

const (
	descClusterVersion  = "Reads the current cluster version from CVO. This is an informational check that always passes; it establishes baseline context alongside health checks."
	descUpgradeInFlight = "Checks whether a cluster version upgrade is currently in progress by inspecting the ClusterVersion Progressing condition. Starting a new upgrade while one is already in-flight can cause conflicts and unpredictable behaviour."
)

type clusterVersionCheck struct{}

func (c *clusterVersionCheck) Name() string                  { return "ClusterVersion" }
func (c *clusterVersionCheck) Description() string            { return descClusterVersion }
func (c *clusterVersionCheck) DefaultTimeout() time.Duration  { return 0 }
func (c *clusterVersionCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}

func (c *clusterVersionCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	cvClient := deps.CvClientBuilder.New(deps.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to get cluster version: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if ClusterVersion cannot be queried.",
		}
	}
	version := ""
	if clusterVersion != nil {
		version = clusterVersion.Status.Desired.Version
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("Cluster version %s", version),
		Findings: []string{fmt.Sprintf("currentVersion=%s", version)},
	}
}

type upgradeInFlightCheck struct{}

func (c *upgradeInFlightCheck) Name() string                  { return "UpgradeInFlight" }
func (c *upgradeInFlightCheck) Description() string            { return descUpgradeInFlight }
func (c *upgradeInFlightCheck) DefaultTimeout() time.Duration  { return 0 }
func (c *upgradeInFlightCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}

func (c *upgradeInFlightCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	cvClient := deps.CvClientBuilder.New(deps.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to get cluster version: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if ClusterVersion cannot be queried.",
		}
	}
	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorProgressing && condition.Status == configv1.ConditionTrue {
			return CheckOutput{
				Result:  upgradev1alpha1.CheckFailed,
				Summary: fmt.Sprintf("Upgrade already in progress: %s", condition.Message),
				Findings: []string{
					fmt.Sprintf("Progressing=True: reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				},
				Owner:  upgradev1alpha1.OwnerServiceTeam,
				Action: "Wait for the in-progress upgrade to complete before starting another. Contact Red Hat support if the upgrade appears stuck.",
			}
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "No upgrade in progress",
		Findings: []string{"ClusterVersion Progressing condition is False (or not set)"},
	}
}
