package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

const (
	muoNamespace  = "openshift-managed-upgrade-operator"
	muoConfigName = "managed-upgrade-config"

	descUpgradeConfigConsistency = "Checks the MUO UpgradeConfig CR for failed history and verifies that the " +
		"CR's desired version is consistent with what the CVO currently has as its desired version. A divergence " +
		"here is the root cause of UpgradeConfigSyncFailureOver4HrSRE alerts. Also surfaces the most-recent " +
		"upgrade history phase and conditions so the cluster admin can see the full upgrade state at a glance. " +
		"Source: MCS-48/ROSAENG-66714 Case 9 (UpgradeConfig sync failure)."
)

type upgradeConfigConsistencyCheck struct{}

func (c *upgradeConfigConsistencyCheck) Name() string        { return "UpgradeConfigConsistency" }
func (c *upgradeConfigConsistencyCheck) Description() string { return descUpgradeConfigConsistency }
func (c *upgradeConfigConsistencyCheck) DefaultTimeout() time.Duration { return 30 * time.Second }
func (c *upgradeConfigConsistencyCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}
func (c *upgradeConfigConsistencyCheck) UpgradePhase() string { return "control-plane" }

func (c *upgradeConfigConsistencyCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	uc := &upgradev1alpha1.UpgradeConfig{}
	err := deps.Client.Get(ctx, types.NamespacedName{Namespace: muoNamespace, Name: muoConfigName}, uc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return CheckOutput{
				Result:  upgradev1alpha1.CheckPassed,
				Summary: "No MUO UpgradeConfig CR found — no upgrade scheduled",
				Findings: []string{
					fmt.Sprintf("namespace=%s name=%s: not found", muoNamespace, muoConfigName),
				},
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to get UpgradeConfig: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if the UpgradeConfig CR cannot be read.",
		}
	}

	var findings []string
	findings = append(findings, fmt.Sprintf("spec.desired: version=%s channel=%s", uc.Spec.Desired.Version, uc.Spec.Desired.Channel))

	// Surface most-recent history entry so the admin can see the full upgrade state.
	var latestPhase upgradev1alpha1.UpgradePhase
	var latestVersion string
	if len(uc.Status.History) > 0 {
		h := uc.Status.History[0]
		latestPhase = h.Phase
		latestVersion = h.Version
		findings = append(findings, fmt.Sprintf("status.history[0]: version=%s phase=%s", latestVersion, string(latestPhase)))
		for i, cond := range h.Conditions {
			if i >= 8 {
				findings = append(findings, fmt.Sprintf("  ... (%d more conditions)", len(h.Conditions)-i))
				break
			}
			findings = append(findings, fmt.Sprintf("  condition: type=%s status=%s message=%s", cond.Type, cond.Status, cond.Message))
		}
	} else {
		findings = append(findings, "status.history: empty (no previous upgrade attempts)")
	}

	// Cross-check spec.desired.version against CVO's current desired version.
	cvClient := deps.CvClientBuilder.New(deps.Client)
	clusterVersion, cvErr := cvClient.GetClusterVersion()
	if cvErr != nil {
		findings = append(findings, fmt.Sprintf("cvo: unable to query ClusterVersion: %v", cvErr))
		// CVO query failed — still check history
	} else {
		cvoDesired := clusterVersion.Status.Desired.Version
		findings = append(findings, fmt.Sprintf("cvo.status.desired.version=%s", cvoDesired))

		if uc.Spec.Desired.Version != "" && cvoDesired != "" && uc.Spec.Desired.Version != cvoDesired {
			return CheckOutput{
				Result: upgradev1alpha1.CheckFailed,
				Summary: fmt.Sprintf("UpgradeConfig desired version %s does not match CVO desired version %s — config sync failure",
					uc.Spec.Desired.Version, cvoDesired),
				Findings: findings,
				Owner:    upgradev1alpha1.OwnerServiceTeam,
				Action:   "Check the MUO controller logs for sync errors (oc logs -n openshift-managed-upgrade-operator deploy/managed-upgrade-operator). This mismatch triggers UpgradeConfigSyncFailureOver4HrSRE. The Red Hat SRE team should verify OCM is sending the correct upgrade target.",
			}
		}
		findings = append(findings, "cvo.status.desired.version matches UpgradeConfig spec: ok")
	}

	// Warn if the most-recent history shows a failed upgrade attempt.
	if latestPhase == upgradev1alpha1.UpgradePhaseFailed {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckResultWarning,
			Summary: fmt.Sprintf("Most recent upgrade attempt failed: version=%s phase=Failed", latestVersion),
			Findings: findings,
			Action:  "Review the upgrade history conditions above for the failure reason. Contact Red Hat support if the failure is unexpected or recurring.",
		}
	}

	summary := fmt.Sprintf("UpgradeConfig is consistent: desired version %s matches CVO", uc.Spec.Desired.Version)
	if latestPhase != "" {
		summary = fmt.Sprintf("%s, last history phase=%s", summary, string(latestPhase))
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  summary,
		Findings: findings,
	}
}
