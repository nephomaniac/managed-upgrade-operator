package upgradediags

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	descUpgradeTarget = "Validates that the requested target version and channel represent a valid upgrade " +
		"from the cluster's current version. For y-stream upgrades (channel change) it confirms the upgrade " +
		"edge exists in the Cincinnati graph. For z-stream upgrades (same channel) it confirms the version " +
		"appears in the CVO's available or conditional updates."

	descUpgradeTargetExtended = "Extended upgrade target assessment that queries both the CVO's cached update " +
		"graph and the live Cincinnati service, then compares the two. Surfaces upgrade type classification, " +
		"cache vs live discrepancies, and full conditional update risk details so the consumer can make an " +
		"informed upgrade decision."
)

// --- UpgradeTarget (baseline) ---

type upgradeTargetCheck struct{}

func (c *upgradeTargetCheck) Name() string                  { return "UpgradeTarget" }
func (c *upgradeTargetCheck) Description() string           { return descUpgradeTarget }
func (c *upgradeTargetCheck) DefaultTimeout() time.Duration { return 0 }
func (c *upgradeTargetCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *upgradeTargetCheck) UpgradePhase() string { return "control-plane" }

func (c *upgradeTargetCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
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

	result, err := validation.ValidateUpgradeTarget(deps.DesiredVersion, deps.DesiredChannel, clusterVersion, logger)
	if err != nil {
		if result.IsValid {
			return CheckOutput{
				Result:  upgradev1alpha1.CheckFailed,
				Summary: result.Message,
				Findings: []string{
					fmt.Sprintf("targetVersion=%s", deps.DesiredVersion),
					fmt.Sprintf("targetChannel=%s", deps.DesiredChannel),
					fmt.Sprintf("reason=%s", err.Error()),
				},
				Action: "Ensure the target version is a valid upgrade from the current cluster version. Downgrades and same-version requests are not supported.",
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("upgrade target validation error: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if the upgrade graph cannot be queried.",
		}
	}

	if !result.IsAvailableUpdate {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckFailed,
			Summary: result.Message,
			Findings: []string{
				fmt.Sprintf("targetVersion=%s", deps.DesiredVersion),
				fmt.Sprintf("targetChannel=%s", deps.DesiredChannel),
			},
			Action: "Verify the target version is available in the specified channel. Check the OpenShift release notes for supported upgrade paths.",
		}
	}

	return CheckOutput{
		Result:  upgradev1alpha1.CheckPassed,
		Summary: fmt.Sprintf("Version %s is a valid upgrade target on channel %s", deps.DesiredVersion, deps.DesiredChannel),
		Findings: []string{
			fmt.Sprintf("targetVersion=%s", deps.DesiredVersion),
			fmt.Sprintf("targetChannel=%s", deps.DesiredChannel),
		},
	}
}

// --- UpgradeTargetExtended ---

type upgradeTargetExtendedCheck struct{}

func (c *upgradeTargetExtendedCheck) Name() string                  { return "UpgradeTargetExtended" }
func (c *upgradeTargetExtendedCheck) Description() string           { return descUpgradeTargetExtended }
func (c *upgradeTargetExtendedCheck) DefaultTimeout() time.Duration { return 60 * time.Second }
func (c *upgradeTargetExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *upgradeTargetExtendedCheck) UpgradePhase() string { return "control-plane" }

func (c *upgradeTargetExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
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

	currentVersion, _ := cv.GetCurrentVersion(clusterVersion)
	currentChannel := clusterVersion.Spec.Channel
	upgradeType := classifyUpgradeType(currentVersion, deps.DesiredVersion, deps.DesiredChannel)

	findings := []string{
		fmt.Sprintf("upgradeType=%s", upgradeType),
		fmt.Sprintf("currentVersion=%s → targetVersion=%s", currentVersion, deps.DesiredVersion),
		fmt.Sprintf("currentChannel=%s → targetChannel=%s", currentChannel, deps.DesiredChannel),
	}

	// CVO cache lookup
	cvoAvail := cvoAvailableVersion(deps.DesiredVersion, clusterVersion.Status.AvailableUpdates)
	cvoCond := cvoConditionalVersion(deps.DesiredVersion, clusterVersion.Status.ConditionalUpdates)
	cvoStatus, cvoRiskFindings := describeCVOStatus(deps.DesiredVersion, cvoAvail, cvoCond)
	findings = append(findings, fmt.Sprintf("cvoCache=%s", cvoStatus))

	// Live Cincinnati query
	cinResult, cinErr := validation.FetchCincinnatiUpdates(ctx, clusterVersion, deps.DesiredChannel)

	var cinAvail bool
	var cinCond *configv1.ConditionalUpdate
	cinStatus := "unreachable"
	var cinRiskFindings []string

	if cinErr == nil {
		cinAvail = cincinnatiAvailableVersion(deps.DesiredVersion, cinResult.Available)
		cinCond = cincinnatiConditionalVersion(deps.DesiredVersion, cinResult.Conditional)
		cinStatus, cinRiskFindings = describeCincinnatiStatus(deps.DesiredVersion, cinAvail, cinCond)
	}
	findings = append(findings, fmt.Sprintf("cincinnatiLive=%s", cinStatus))

	// Append risk details — prefer live Cincinnati data, fall back to CVO cache
	riskFindings := cinRiskFindings
	if len(riskFindings) == 0 {
		riskFindings = cvoRiskFindings
	}
	findings = append(findings, riskFindings...)

	// --- Result determination ---

	// Hard fail: risk applies to this cluster
	if cond := firstConditional(cinCond, cvoCond); cond != nil {
		if recommended, evaluated, msg := conditionalRecommended(cond); evaluated && !recommended {
			return CheckOutput{
				Result:   upgradev1alpha1.CheckFailed,
				Summary:  fmt.Sprintf("Conditional update risk applies to this cluster: %s", msg),
				Findings: findings,
				Action:   "Review the risk details above and resolve the flagged condition before upgrading. Contact Red Hat support for assistance.",
			}
		}
	}

	// Hard fail: not found in either source
	if cinErr == nil && cinStatus == "not-found" && cvoStatus == "not-found" {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Version %s not found in Cincinnati or CVO cache on channel %s", deps.DesiredVersion, deps.DesiredChannel),
			Findings: findings,
			Action:   "Verify the target version and channel are correct. Check available versions in the OpenShift release notes.",
		}
	}

	// Warning: in CVO cache but Cincinnati does not confirm
	if cinErr == nil && !cinAvail && cinCond == nil && (cvoAvail || cvoCond != nil) {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultWarning,
			Summary:  fmt.Sprintf("Version %s found in CVO cache but not confirmed by live Cincinnati — cache may be stale or version removed from graph", deps.DesiredVersion),
			Findings: findings,
			Action:   "Verify this version is still available on the channel before upgrading.",
		}
	}

	// Warning: Cincinnati unreachable — CVO cache only
	if cinErr != nil {
		findings = append(findings, fmt.Sprintf("cincinnatiError=%v", cinErr))
		if cvoAvail || cvoCond != nil {
			return CheckOutput{
				Result:   upgradev1alpha1.CheckResultWarning,
				Summary:  "Live Cincinnati query failed — upgrade target assessment based on CVO cache only",
				Findings: findings,
				Action:   "Verify network connectivity to the Cincinnati update service. CVO cache data may be stale.",
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("Cincinnati unreachable and version not in CVO cache: %v", cinErr),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Check network connectivity to the update service. Contact Red Hat support if the issue persists.",
		}
	}

	// Warning: Cincinnati confirms but CVO cache stale
	if !cvoAvail && cvoCond == nil && (cinAvail || cinCond != nil) {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckResultWarning,
			Summary:  fmt.Sprintf("Version %s confirmed by Cincinnati but not yet reflected in CVO cache — cache may be stale", deps.DesiredVersion),
			Findings: findings,
		}
	}

	// Passed — build informative summary
	summary := fmt.Sprintf("Version %s is a valid %s upgrade target on channel %s", deps.DesiredVersion, upgradeType, deps.DesiredChannel)
	if cinCond != nil || cvoCond != nil {
		summary = fmt.Sprintf("Version %s is a conditional %s upgrade — risks present but do not apply to this cluster", deps.DesiredVersion, upgradeType)
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  summary,
		Findings: findings,
	}
}

// --- helpers ---

func classifyUpgradeType(currentVersion, targetVersion, targetChannel string) string {
	if strings.Contains(targetChannel, "eus") {
		return "eus"
	}
	cur, errC := semver.Parse(currentVersion)
	tgt, errT := semver.Parse(targetVersion)
	if errC == nil && errT == nil && (cur.Major != tgt.Major || cur.Minor != tgt.Minor) {
		return "y-stream"
	}
	return "z-stream"
}

func cvoAvailableVersion(version string, updates []configv1.Release) bool {
	for _, u := range updates {
		if u.Version == version {
			return true
		}
	}
	return false
}

func cvoConditionalVersion(version string, updates []configv1.ConditionalUpdate) *configv1.ConditionalUpdate {
	for i := range updates {
		if updates[i].Release.Version == version {
			return &updates[i]
		}
	}
	return nil
}

func cincinnatiAvailableVersion(version string, releases []configv1.Release) bool {
	for _, r := range releases {
		if r.Version == version {
			return true
		}
	}
	return false
}

func cincinnatiConditionalVersion(version string, conditionals []configv1.ConditionalUpdate) *configv1.ConditionalUpdate {
	for i := range conditionals {
		if conditionals[i].Release.Version == version {
			return &conditionals[i]
		}
	}
	return nil
}

func conditionalRecommended(u *configv1.ConditionalUpdate) (recommended, evaluated bool, message string) {
	for _, cond := range u.Conditions {
		if cond.Type == "Recommended" {
			return cond.Status == metav1.ConditionTrue, true, cond.Message
		}
	}
	return false, false, ""
}

func firstConditional(a, b *configv1.ConditionalUpdate) *configv1.ConditionalUpdate {
	if a != nil {
		return a
	}
	return b
}

func riskFindings(update *configv1.ConditionalUpdate) []string {
	if update == nil {
		return nil
	}
	var out []string
	for _, risk := range update.Risks {
		applies := "unknown"
		for _, cond := range risk.Conditions {
			if cond.Type == "Applies" {
				if cond.Status == metav1.ConditionTrue {
					applies = "YES — applies to this cluster"
				} else {
					applies = "no"
				}
			}
		}
		out = append(out, fmt.Sprintf("risk=%s applies=%s message=%q url=%s", risk.Name, applies, risk.Message, risk.URL))
	}
	return out
}

func describeCVOStatus(version string, avail bool, cond *configv1.ConditionalUpdate) (status string, risks []string) {
	if avail {
		return "available", nil
	}
	if cond != nil {
		recommended, evaluated, _ := conditionalRecommended(cond)
		switch {
		case !evaluated:
			return "conditional (risks not yet evaluated)", riskFindings(cond)
		case recommended:
			return "conditional (recommended)", riskFindings(cond)
		default:
			return "conditional (not recommended — risk applies to this cluster)", riskFindings(cond)
		}
	}
	return "not-found", nil
}

func describeCincinnatiStatus(version string, avail bool, cond *configv1.ConditionalUpdate) (status string, risks []string) {
	return describeCVOStatus(version, avail, cond)
}
