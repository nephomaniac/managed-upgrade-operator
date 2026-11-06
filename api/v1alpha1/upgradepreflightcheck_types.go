package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpgradePreflightCheckSpec defines the target upgrade to check readiness for.
// It embeds UpgradeConfigSpec so that all UpgradeConfig fields (type, desired version,
// PDBForceDrainTimeout, etc.) are inherited automatically. Fields like upgradeAt and
// capacityReservation are present but unused by the preflight controller.
type UpgradePreflightCheckSpec struct {
	UpgradeConfigSpec `json:",inline"`

	// DefaultCheckTimeoutSeconds is the per-check timeout fallback used when a check
	// does not declare its own timeout. Defaults to 30 seconds.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=30
	DefaultCheckTimeoutSeconds int32 `json:"defaultCheckTimeoutSeconds,omitempty"`

	// OverallTimeoutSeconds is the maximum total wall-clock time allowed for all checks
	// to complete. If this deadline is reached, any checks that have not yet started are
	// recorded with Result=TimedOut. Checks already running receive a cancelled context
	// and are recorded as Result=Error. When 0 (default), there is no overall deadline.
	// +kubebuilder:validation:Optional
	OverallTimeoutSeconds int32 `json:"overallTimeoutSeconds,omitempty"`

	// Checks is an optional allowlist of check names to run. When empty, all checks run.
	// Checks not in this list are recorded with Result=Skipped.
	// +kubebuilder:validation:Optional
	Checks []string `json:"checks,omitempty"`
}

// CheckExecutionState describes whether a check was able to run.
// +kubebuilder:validation:Enum={"Completed","Error","Skipped","TimedOut"}
type CheckExecutionState string

const (
	// CheckCompleted means the check ran to completion; see Result for the health verdict.
	CheckCompleted CheckExecutionState = "Completed"
	// CheckError means the check could not run due to an infrastructure or transient error.
	CheckError CheckExecutionState = "Error"
	// CheckSkipped means the check was excluded by the Checks allowlist in the spec.
	CheckSkipped CheckExecutionState = "Skipped"
	// CheckTimedOut means the check never started because the overall run timeout was reached.
	CheckTimedOut CheckExecutionState = "TimedOut"
)

// CheckOwner identifies who is responsible for resolving a failed or errored check.
// +kubebuilder:validation:Enum={"customer","service-team"}
type CheckOwner string

const (
	// OwnerCustomer means the customer must take action to resolve this check.
	OwnerCustomer CheckOwner = "customer"
	// OwnerServiceTeam means the Red Hat service team must investigate.
	OwnerServiceTeam CheckOwner = "service-team"
)

// CheckResult is the top-level health verdict for a preflight check.
// +kubebuilder:validation:Enum={"Passed","Failed","Error","Skipped","TimedOut","Unknown"}
type CheckResult string

const (
	// CheckPassed means the cluster is healthy for this check dimension.
	CheckPassed CheckResult = "Passed"
	// CheckFailed means the cluster has a health issue that may block an upgrade.
	CheckFailed CheckResult = "Failed"
	// CheckResultError means the check could not determine cluster health (infrastructure/transient failure).
	CheckResultError CheckResult = "Error"
	// CheckResultSkipped means the check was not run (excluded by the Checks allowlist).
	CheckResultSkipped CheckResult = "Skipped"
	// CheckResultTimedOut means the check never started because the overall run deadline was exceeded.
	CheckResultTimedOut CheckResult = "TimedOut"
	// CheckResultUnknown is the initial state before a check has run.
	CheckResultUnknown CheckResult = "Unknown"
)

// CheckExecution describes the runtime execution of a single preflight check.
type CheckExecution struct {
	// State is whether the check machinery ran successfully: Completed, Error, or Skipped.
	State CheckExecutionState `json:"state"`

	// StartedAt is when this individual check began executing.
	// +kubebuilder:validation:Optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when this individual check finished executing.
	// +kubebuilder:validation:Optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Summary is a concise human-readable explanation of what the check found.
	// Examples:
	//   Passed: "All 34 cluster operators healthy"
	//   Failed: "2 degraded operators found"
	//   Error:  "" (see Error field instead)
	// +kubebuilder:validation:Optional
	Summary string `json:"summary,omitempty"`

	// Findings contains detailed per-item observations from the check.
	// Examples:
	//   Passed: ["authentication: Available=True, Degraded=False", ...]
	//   Failed: ["kube-apiserver: Degraded=True", "etcd: Degraded=True"]
	//   Error:  []
	// +kubebuilder:validation:Optional
	Findings []string `json:"findings,omitempty"`

	// Error contains the reason a check could not run (State=Error only).
	// This is an infrastructure/transient failure, not a cluster health finding.
	// +kubebuilder:validation:Optional
	Error string `json:"error,omitempty"`
}

// PreflightCheckResult holds the result and execution details for a single preflight check.
type PreflightCheckResult struct {
	// Name identifies the check (e.g. "CriticalAlerts", "ClusterOperators").
	Name string `json:"name"`

	// Description is a human-readable explanation of what this check validates and why it matters.
	// +kubebuilder:validation:Optional
	Description string `json:"description,omitempty"`

	// Result is the health verdict: Passed, Failed, Error, Skipped, or Unknown.
	// This is the top-level signal — no need to inspect Execution to determine check outcome.
	Result CheckResult `json:"result"`

	// Owner identifies who is responsible for resolving this check when it fails or errors.
	// "customer" means the customer must act; "service-team" means Red Hat must investigate.
	// +kubebuilder:validation:Optional
	Owner CheckOwner `json:"owner,omitempty"`

	// Action is the recommended next step when Result is Failed or Error.
	// Empty when the check passed or was skipped.
	// +kubebuilder:validation:Optional
	Action string `json:"action,omitempty"`

	// TimeoutSeconds is the timeout that was applied to this check.
	TimeoutSeconds int32 `json:"timeoutSeconds"`

	// Execution contains runtime information about how the check ran.
	Execution CheckExecution `json:"execution"`
}

// UpgradePreflightCheckStatus reports the outcome of all checks.
type UpgradePreflightCheckStatus struct {
	// +kubebuilder:validation:Optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +kubebuilder:validation:Optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +kubebuilder:validation:Optional
	Checks []PreflightCheckResult `json:"checks,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=upgradepreflightchecks,scope=Namespaced,shortName=pfc
// +kubebuilder:printcolumn:name="version",type="string",JSONPath=".spec.desired.version"
// +kubebuilder:printcolumn:name="channel",type="string",JSONPath=".spec.desired.channel"
// +kubebuilder:printcolumn:name="type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="started",type="string",JSONPath=".status.startedAt"
// +kubebuilder:printcolumn:name="completed",type="string",JSONPath=".status.completedAt"

// UpgradePreflightCheck is the Schema for on-demand cluster upgrade readiness checks.
// Creating one runs pre-upgrade health checks without scheduling or starting an upgrade.
type UpgradePreflightCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpgradePreflightCheckSpec   `json:"spec,omitempty"`
	Status UpgradePreflightCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UpgradePreflightCheckList contains a list of UpgradePreflightCheck.
type UpgradePreflightCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpgradePreflightCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UpgradePreflightCheck{}, &UpgradePreflightCheckList{})
}
