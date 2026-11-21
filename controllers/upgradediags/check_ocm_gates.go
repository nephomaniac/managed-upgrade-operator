package upgradediags

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
)

const (
	descOCMVersionGates = "Checks whether all OCM version gate prerequisites have been acknowledged for every " +
		"minor version boundary between the current cluster version and the target version. " +
		"When upgrading across multiple minor versions (e.g. 4.22 → 4.27), gates for each boundary " +
		"(4.23, 4.24, 4.25, 4.26, 4.27) are checked. Unacknowledged gates — such as API removal notices, " +
		"STS permission updates, or ingress change acknowledgements — will block the upgrade at that boundary."
)

type ocmVersionGateCheck struct{}

func (c *ocmVersionGateCheck) Name() string                  { return "OCMVersionGates" }
func (c *ocmVersionGateCheck) Description() string           { return descOCMVersionGates }
func (c *ocmVersionGateCheck) DefaultTimeout() time.Duration { return 60 * time.Second }
func (c *ocmVersionGateCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *ocmVersionGateCheck) UpgradePhase() string { return "pre-checks" }

func (c *ocmVersionGateCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
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
			Summary:  "OCM not configured — version gate check skipped",
			Findings: []string{"OCM base URL not set in MUO config; this check requires OCM connectivity"},
		}
	}

	// Resolve current cluster version
	cvClient := deps.CvClientBuilder.New(deps.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to get cluster version: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
		}
	}
	currentVersion, _ := cv.GetCurrentVersion(clusterVersion)

	currentMajor, currentMinor, err := parseMajorMinorFromVersion(currentVersion)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("can't parse current version %q: %v", currentVersion, err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
		}
	}
	targetMajor, targetMinor, err := parseMajorMinorFromVersion(deps.DesiredVersion)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("can't parse target version %q: %v", deps.DesiredVersion, err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
		}
	}

	// Gate boundaries only apply to forward y-stream upgrades within the same major version
	if targetMajor != currentMajor || targetMinor <= currentMinor {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckResultSkipped,
			Summary: fmt.Sprintf("Target %s is not a y-stream forward upgrade from %s — version gate boundary check not applicable", deps.DesiredVersion, currentVersion),
			Findings: []string{
				fmt.Sprintf("currentVersion=%s targetVersion=%s", currentVersion, deps.DesiredVersion),
			},
		}
	}

	// Get cluster from OCM for ID and type attributes used in gate filtering
	cluster, err := ocmClient.GetCluster()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get cluster from OCM: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check OCM connectivity. Contact Red Hat support if the issue persists.",
		}
	}

	// Build set of already-acknowledged gate IDs
	agreements, err := ocmClient.GetVersionGateAgreements(cluster.ID())
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to get gate agreements: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if gate agreements cannot be retrieved.",
		}
	}
	ackedGateIDs := make(map[string]bool, len(agreements))
	for _, a := range agreements {
		if gate := a.VersionGate(); gate != nil {
			ackedGateIDs[gate.ID()] = true
		}
	}

	isSTSCluster := clusterIsSTSType(cluster)
	cloudProvider := clusterCloudProviderID(cluster)
	boundaryCount := targetMinor - currentMinor

	// Collect unacknowledged gates across all minor version boundaries
	type pendingGate struct {
		gate     *cmv1.VersionGate
		boundary string
	}
	var pending []pendingGate
	var fetchErrors []string

	for minor := currentMinor + 1; minor <= targetMinor; minor++ {
		boundary := fmt.Sprintf("%d.%d", currentMajor, minor)
		gates, err := ocmClient.GetVersionGates(boundary)
		if err != nil {
			logger.Error(err, "Failed to fetch version gates for boundary", "boundary", boundary)
			fetchErrors = append(fetchErrors, fmt.Sprintf("boundary=%s: %v", boundary, err))
			continue
		}
		for _, gate := range gates {
			if ackedGateIDs[gate.ID()] {
				continue
			}
			if gate.STSOnly() && !isSTSCluster {
				continue
			}
			if gateConditionExcludes(gate.ClusterCondition(), cloudProvider) {
				continue
			}
			pending = append(pending, pendingGate{gate: gate, boundary: boundary})
		}
	}

	// If we couldn't reach some boundaries, report an error so the admin knows the check is incomplete
	if len(fetchErrors) > 0 && len(pending) == 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultError,
			Error:    fmt.Sprintf("failed to retrieve version gates for %d boundary/boundaries: %s", len(fetchErrors), strings.Join(fetchErrors, "; ")),
			Owner:    upgradev1alpha1.OwnerServiceTeam,
			Action:   "Check OCM connectivity. Contact Red Hat support if the issue persists.",
			Findings: fetchErrors,
		}
	}

	if len(pending) == 0 {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckPassed,
			Summary: fmt.Sprintf("All version gate prerequisites satisfied across %d minor version boundary/boundaries (%s → %s)", boundaryCount, currentVersion, deps.DesiredVersion),
			Findings: []string{
				fmt.Sprintf("currentVersion=%s targetVersion=%s boundariesChecked=%d ackedGates=%d", currentVersion, deps.DesiredVersion, boundaryCount, len(ackedGateIDs)),
			},
		}
	}

	// Build one structured finding per unacknowledged gate
	var strFindings []string
	var structFindings []upgradev1alpha1.StructuredFinding

	strFindings = append(strFindings, fmt.Sprintf("currentVersion=%s targetVersion=%s", currentVersion, deps.DesiredVersion))
	strFindings = append(fetchErrors, strFindings...)

	for _, pg := range pending {
		gate := pg.gate
		desc := gate.Description()
		if desc == "" {
			desc = fmt.Sprintf("Unacknowledged version gate for %s (gate %s)", pg.boundary, gate.ID())
		}

		action := "Acknowledge this gate via the OpenShift Cluster Manager or rosa CLI before upgrading."
		if gate.WarningMessage() != "" {
			action = gate.WarningMessage() + " Acknowledge this gate before upgrading."
		}

		strFindings = append(strFindings,
			fmt.Sprintf("boundary=%s gate=%s label=%s description=%q", pg.boundary, gate.ID(), gate.Label(), desc),
		)
		if gate.WarningMessage() != "" {
			strFindings = append(strFindings, fmt.Sprintf("  warning: %s", gate.WarningMessage()))
		}
		if gate.DocumentationURL() != "" {
			strFindings = append(strFindings, fmt.Sprintf("  doc: %s", gate.DocumentationURL()))
		}

		fb := NewFinding(desc).
			WithSeverity(upgradev1alpha1.FindingSeverityHigh).
			WithOwner(upgradev1alpha1.OwnerCustomer).
			WithAction(action).
			WithMetadata("boundary_version", pg.boundary).
			WithMetadata("gate_id", gate.ID()).
			WithMetadata("gate_label", gate.Label())
		if gate.DocumentationURL() != "" {
			fb = fb.WithDocumentation(gate.DocumentationURL())
		}
		structFindings = append(structFindings, fb.Build())
	}

	return CheckOutput{
		Result:             upgradev1alpha1.CheckFailed,
		Summary:            fmt.Sprintf("%d version gate(s) not yet acknowledged for upgrade path %s → %s", len(pending), currentVersion, deps.DesiredVersion),
		Findings:           strFindings,
		StructuredFindings: structFindings,
		Action:             "Acknowledge all listed version gates via the OpenShift Cluster Manager console or the rosa CLI before proceeding with the upgrade.",
	}
}

// parseMajorMinorFromVersion extracts major and minor integers from "X.Y.Z" or "X.Y".
func parseMajorMinorFromVersion(version string) (int, int, error) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected at least X.Y format, got %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid major version in %q: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid minor version in %q: %w", version, err)
	}
	return major, minor, nil
}

// clusterIsSTSType returns true if the cluster uses STS authentication.
func clusterIsSTSType(cluster *cmv1.Cluster) bool {
	if cluster == nil {
		return false
	}
	aws := cluster.AWS()
	if aws == nil {
		return false
	}
	sts := aws.STS()
	return sts != nil && sts.Enabled()
}

// clusterCloudProviderID returns the cloud provider identifier (e.g. "aws", "gcp").
func clusterCloudProviderID(cluster *cmv1.Cluster) string {
	if cluster == nil {
		return ""
	}
	cp := cluster.CloudProvider()
	if cp == nil {
		return ""
	}
	return cp.ID()
}

// gateConditionExcludes returns true if the gate's cluster_condition indicates it only
// applies to a specific cloud provider that this cluster does not use.
// Conservative: returns false (include the gate) whenever the condition cannot be parsed.
func gateConditionExcludes(condition, cloudProvider string) bool {
	if condition == "" {
		return false
	}
	// "gcp.<attr>" — applies to GCP only
	if strings.HasPrefix(condition, "gcp.") && cloudProvider != "gcp" {
		return true
	}
	// "aws.<attr>" — applies to AWS only
	if strings.HasPrefix(condition, "aws.") && cloudProvider != "aws" {
		return true
	}
	return false
}
