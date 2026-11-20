package upgradediags

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const descInstalledOperatorMaxVersion = "Scans all OLM-managed ClusterServiceVersions for the olm.maxOpenShiftVersion annotation. Flags any operator whose declared maximum supported OCP version is less than the target upgrade version — these operators will block upgrade progress. Source: OHSS Case 5 (OHSS-53041: cluster-logging blocking 4.19 upgrade)."

var csvListGVK = schema.GroupVersionKind{Group: "operators.coreos.com", Version: "v1alpha1", Kind: "ClusterServiceVersionList"}

type installedOperatorMaxVersionCheck struct{}

func (c *installedOperatorMaxVersionCheck) Name() string { return "InstalledOperatorMaxVersion" }
func (c *installedOperatorMaxVersionCheck) Description() string {
	return descInstalledOperatorMaxVersion
}
func (c *installedOperatorMaxVersionCheck) DefaultTimeout() time.Duration { return 60 * time.Second }
func (c *installedOperatorMaxVersionCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *installedOperatorMaxVersionCheck) UpgradePhase() string { return "operators" }

func (c *installedOperatorMaxVersionCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	// Parse target minor version (e.g. "4.19.3" → 4, 19)
	targetMajor, targetMinor, err := parseMajorMinor(deps.DesiredVersion)
	if err != nil {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckPassed,
			Summary:  "Skipped: unable to parse target version for operator compatibility check",
			Findings: []string{fmt.Sprintf("DesiredVersion %q is not a valid semver: %v", deps.DesiredVersion, err)},
		}
	}

	csvList := &unstructured.UnstructuredList{}
	csvList.SetGroupVersionKind(csvListGVK)
	if err := deps.Client.List(ctx, csvList, &client.ListOptions{}); err != nil {
		if apierrors.IsForbidden(err) {
			return CheckOutput{
				Result:   upgradev1alpha1.CheckResultWarning,
				Summary:  "RBAC not granted — cannot list ClusterServiceVersions",
				Findings: []string{fmt.Sprintf("forbidden: %v", err)},
				Owner:    upgradev1alpha1.OwnerServiceTeam,
				Action:   "Add 'clusterserviceversions' get+list permission for 'operators.coreos.com' to the managed-upgrade-operator ClusterRole.",
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list ClusterServiceVersions: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if OLM operator list cannot be retrieved.",
		}
	}

	const maxVersionAnnotation = "olm.maxOpenShiftVersion"
	var blocked []string
	var findings []string

	for _, csv := range csvList.Items {
		annotations := csv.GetAnnotations()
		maxVer, ok := annotations[maxVersionAnnotation]
		if !ok || maxVer == "" {
			continue
		}
		maxMajor, maxMinor, err := parseMajorMinor(maxVer)
		if err != nil {
			findings = append(findings, fmt.Sprintf("%s/%s: unparseable %s=%q", csv.GetNamespace(), csv.GetName(), maxVersionAnnotation, maxVer))
			continue
		}
		// operator blocks upgrade if its max version < target version
		if maxMajor < targetMajor || (maxMajor == targetMajor && maxMinor < targetMinor) {
			label := fmt.Sprintf("%s/%s: maxOpenShiftVersion=%s blocks upgrade to %s",
				csv.GetNamespace(), csv.GetName(), maxVer, deps.DesiredVersion)
			blocked = append(blocked, label)
			findings = append(findings, label)
		}
	}

	if len(blocked) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d OLM operator(s) declare maxOpenShiftVersion < %s", len(blocked), deps.DesiredVersion),
			Findings: findings,
			Action:   "Update or remove the flagged operator(s) before upgrading. Check OperatorHub for a newer version that supports the target OCP release.",
		}
	}
	return CheckOutput{
		Result:  upgradev1alpha1.CheckPassed,
		Summary: fmt.Sprintf("No OLM operators with incompatible maxOpenShiftVersion for %s (checked %d CSVs)", deps.DesiredVersion, len(csvList.Items)),
		Findings: []string{
			fmt.Sprintf("Scanned %d ClusterServiceVersion(s): none declare olm.maxOpenShiftVersion < %s", len(csvList.Items), deps.DesiredVersion),
		},
	}
}

// parseMajorMinor extracts the major and minor integer components from a
// version string of the form "X.Y" or "X.Y.Z".
func parseMajorMinor(version string) (int, int, error) {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("expected at least X.Y, got %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid major %q: %w", parts[0], err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid minor %q: %w", parts[1], err)
	}
	return major, minor, nil
}
