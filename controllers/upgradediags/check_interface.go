package upgradediags

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	cv "github.com/openshift/managed-upgrade-operator/pkg/clusterversion"
	"github.com/openshift/managed-upgrade-operator/pkg/dvo"
	"github.com/openshift/managed-upgrade-operator/pkg/machinery"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	"github.com/openshift/managed-upgrade-operator/pkg/ocm"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckDependencies provides cluster clients to check implementations.
// Every field a check might need is here; use only what your check requires.
type CheckDependencies struct {
	Client               client.Client
	MetricsClientBuilder metrics.MetricsBuilder
	CvClientBuilder      cv.ClusterVersionBuilder
	Machinery            machinery.Machinery
	DvoClientBuilder     dvo.DvoClientBuilder
	OcmClientBuilder     ocm.OcmClientBuilder
	// DesiredVersion is the target upgrade version from the CR spec.
	DesiredVersion string
	// DesiredChannel is the target upgrade channel from the CR spec.
	DesiredChannel string
}

// CheckOutput is returned by ReadinessCheck.Run.
// The framework converts this into a full DiagsCheckResult — the check author
// does not call withTiming, withDesc, or withOwner directly.
type CheckOutput struct {
	// Result is CheckPassed, CheckFailed, or CheckResultError.
	Result upgradev1alpha1.CheckResult
	// Summary is a one-line verdict (e.g. "3 degraded operators found").
	Summary string
	// Findings are per-item details shown in the CR status (string form for backwards compat).
	Findings []string
	// Error is the reason the check machinery failed (Result=CheckResultError only).
	Error string
	// Owner overrides DefaultOwner() for this specific result path.
	// Leave empty to use the check's DefaultOwner().
	Owner upgradev1alpha1.CheckOwner
	// Action is the recommended next step. Set this on Failed and Error results.
	Action string
	// StructuredFindings provides rich, machine-readable per-finding details.
	// When set, the framework auto-populates string Findings from Message fields
	// if Findings is empty, maintaining backwards compatibility.
	StructuredFindings []upgradev1alpha1.StructuredFinding
}

// ReadinessCheck is the contract every readiness check must satisfy.
//
// To add a new check:
//  1. Create a struct implementing this interface (add it to the appropriate
//     check_<group>.go file, or create a new one).
//  2. Register it in checks.go by appending to registeredChecks.
//
// The framework handles: timeout context, panic recovery, timing, Description,
// DefaultOwner, and UpgradePhase injection. Your Run() only needs to return a CheckOutput.
type DiagnosticsCheck interface {
	// Name returns the unique check identifier used in CR status and the allowlist.
	Name() string
	// Description returns a human-readable explanation of what this check validates.
	Description() string
	// DefaultOwner returns who is responsible when this check fails or errors
	// and Run() does not set CheckOutput.Owner.
	DefaultOwner() upgradev1alpha1.CheckOwner
	// UpgradePhase returns the upgrade phase this check most directly validates.
	// Valid values: "pre-checks", "control-plane", "operators", "worker-upgrade".
	// The framework injects this into every DiagsCheckResult so UIs can correlate
	// findings with the phase where they manifest — without any hardcoded mapping.
	UpgradePhase() string
	// DefaultTimeout returns the preferred per-check timeout. Return 0 to inherit
	// the spec.defaultCheckTimeoutSeconds value (or the global 30s fallback).
	// Override this for checks that are known to be slow (e.g. PDB node scans).
	DefaultTimeout() time.Duration
	// Run executes the check. ctx already has the timeout applied.
	// Set CheckOutput.Action on Failed and Error results.
	Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput
}
