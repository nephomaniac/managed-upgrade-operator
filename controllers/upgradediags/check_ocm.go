package upgradediags

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	muocfg "github.com/openshift/managed-upgrade-operator/config"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/configmanager"
	"github.com/openshift/managed-upgrade-operator/pkg/ocm"
	"github.com/openshift/managed-upgrade-operator/pkg/ocmprovider"
)

const (
	descOCMLimitedSupport = "Queries OCM for active Limited Support reasons on this cluster. " +
		"Limited Support means the cluster has drifted from a supported configuration; it may " +
		"block OCM-managed upgrades until resolved. Source: MCS-1507/1508."

	descOCMUpgradePolicy = "Queries OCM for the cluster's upgrade policy state. Surfaces delayed, " +
		"cancelled, or failed upgrade scheduling — including cases where spec.pausedUntil or " +
		"other OCM-side fields block the upgrade from starting. Source: MCS-1510."
)

// ocmClientFromDeps builds an OCM client from the standard MUO config configmap.
// Returns nil, nil if OCM is not configured (non-fatal — check skips gracefully).
func ocmClientFromDeps(ctx context.Context, deps CheckDependencies) (ocm.OcmClient, error) {
	if deps.OcmClientBuilder == nil {
		return nil, nil
	}

	target := muocfg.CMTarget{}
	cmTarget, err := target.NewCMTarget()
	if err != nil {
		return nil, fmt.Errorf("can't resolve MUO config target: %w", err)
	}

	cfg := &ocmprovider.OcmProviderConfig{}
	cm := configmanager.NewBuilder().New(deps.Client, cmTarget)
	if err := cm.Into(cfg); err != nil {
		// Config not available — OCM not configured, skip gracefully.
		return nil, nil
	}

	ocmURL := cfg.GetOCMBaseURL()
	if ocmURL == nil || ocmURL.String() == "" {
		return nil, nil
	}

	client, err := deps.OcmClientBuilder.New(deps.Client, ocmURL)
	if err != nil {
		return nil, fmt.Errorf("can't build OCM client: %w", err)
	}
	return client, nil
}

// --- OCMLimitedSupport ---

type ocmLimitedSupportCheck struct{}

func (c *ocmLimitedSupportCheck) Name() string                 { return "OCMLimitedSupport" }
func (c *ocmLimitedSupportCheck) Description() string          { return descOCMLimitedSupport }
func (c *ocmLimitedSupportCheck) DefaultTimeout() time.Duration { return 30 * time.Second }
func (c *ocmLimitedSupportCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}
func (c *ocmLimitedSupportCheck) UpgradePhase() string { return "pre-checks" }

func (c *ocmLimitedSupportCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	ocmClient, err := ocmClientFromDeps(ctx, deps)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to connect to OCM: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check OCM connectivity and MUO configuration.",
		}
	}
	if ocmClient == nil {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultSkipped,
			Summary:  "OCM not configured — Limited Support check skipped",
			Findings: []string{"OCM base URL not set in MUO config; this check requires OCM connectivity"},
		}
	}

	cluster, err := ocmClient.GetCluster()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get cluster from OCM: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check OCM connectivity. Contact Red Hat support if the issue persists.",
		}
	}

	reasons, err := ocmClient.GetLimitedSupportReasons(cluster.ID())
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get Limited Support reasons: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if Limited Support reasons cannot be retrieved.",
		}
	}

	if len(reasons) == 0 {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckPassed,
			Summary: "No Limited Support reasons found — cluster is in full support",
		}
	}

	var findings []string
	var summaries []string
	for _, r := range reasons {
		summary := r.Summary()
		details := r.Details()
		findings = append(findings, fmt.Sprintf("reason: %s", summary))
		if details != "" {
			findings = append(findings, fmt.Sprintf("  details: %s", details))
		}
		summaries = append(summaries, summary)
	}

	// Try to classify: version-EOL LS should prompt upgrade, not block it.
	// Until MCS-1507 adds structured type/category to OCM, we use text heuristics.
	isVersionEOL := false
	for _, s := range summaries {
		sl := strings.ToLower(s)
		if strings.Contains(sl, "end of life") || strings.Contains(sl, "end-of-life") ||
			strings.Contains(sl, "eol") || strings.Contains(sl, "version support") {
			isVersionEOL = true
			break
		}
	}

	if isVersionEOL {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultWarning,
			Summary:  fmt.Sprintf("Cluster is in Limited Support due to version end-of-life (%d reason(s)) — upgrading will resolve this", len(reasons)),
			Findings: findings,
			Action:   "Upgrade the cluster to a supported version to exit Limited Support.",
		}
	}

	return CheckOutput{
		Result:  upgradev1alpha1.CheckFailed,
		Summary: fmt.Sprintf("Cluster is in Limited Support with %d active reason(s) — upgrade may be blocked until resolved", len(reasons)),
		Findings: findings,
		Action:  "Review the Limited Support reasons above and contact Red Hat support to resolve them before upgrading.",
	}
}

// --- OCMUpgradePolicy ---

type ocmUpgradePolicyCheck struct{}

func (c *ocmUpgradePolicyCheck) Name() string                 { return "OCMUpgradePolicy" }
func (c *ocmUpgradePolicyCheck) Description() string          { return descOCMUpgradePolicy }
func (c *ocmUpgradePolicyCheck) DefaultTimeout() time.Duration { return 30 * time.Second }
func (c *ocmUpgradePolicyCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}
func (c *ocmUpgradePolicyCheck) UpgradePhase() string { return "pre-checks" }

func (c *ocmUpgradePolicyCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	ocmClient, err := ocmClientFromDeps(ctx, deps)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to connect to OCM: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check OCM connectivity and MUO configuration.",
		}
	}
	if ocmClient == nil {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultSkipped,
			Summary:  "OCM not configured — upgrade policy check skipped",
			Findings: []string{"OCM base URL not set in MUO config; this check requires OCM connectivity"},
		}
	}

	cluster, err := ocmClient.GetCluster()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get cluster from OCM: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check OCM connectivity. Contact Red Hat support if the issue persists.",
		}
	}

	policies, err := ocmClient.GetClusterUpgradePolicies(cluster.ID())
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get upgrade policies: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if upgrade policies cannot be retrieved.",
		}
	}

	if policies.Total() == 0 {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckPassed,
			Summary: "No active upgrade policies found in OCM",
		}
	}

	var findings []string
	var blocked []string

	policies.Items().Each(func(p *cmv1.UpgradePolicy) bool {
		policyID := p.ID()
		nextRun := p.NextRun()
		version := p.Version()

		state, stateErr := ocmClient.GetClusterUpgradePolicyState(policyID, cluster.ID())
		if stateErr != nil {
			findings = append(findings, fmt.Sprintf("policy=%s version=%s: could not get state: %v", policyID, version, stateErr))
			return true
		}

		stateVal := string(state.Value())
		desc := state.Description()
		finding := fmt.Sprintf("policy=%s version=%s state=%s nextRun=%s", policyID, version, stateVal, nextRun.Format(time.RFC3339))
		if desc != "" {
			finding += fmt.Sprintf(" description=%q", desc)
		}
		findings = append(findings, finding)

		switch stateVal {
		case "delayed", "cancelled", "failed":
			msg := fmt.Sprintf("policy for %s is %s", version, stateVal)
			if desc != "" {
				msg += ": " + desc
			}
			blocked = append(blocked, msg)
		}
		return true
	})

	if len(blocked) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("OCM upgrade policy blocked: %s", strings.Join(blocked, "; ")),
			Findings: findings,
			Action:   "Review the upgrade policy state in OCM. If spec.pausedUntil is set (e.g. by Terraform), clear it to unblock the upgrade schedule.",
		}
	}

	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("OCM upgrade policy state is healthy (%d policy/policies)", policies.Total()),
		Findings: findings,
	}
}
