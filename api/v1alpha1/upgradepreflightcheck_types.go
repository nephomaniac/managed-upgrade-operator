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

	// CheckTimeoutSeconds is the maximum number of seconds each individual check
	// may run before it is cancelled and recorded as Result=Error.
	// Defaults to 30 seconds.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=30
	CheckTimeoutSeconds int32 `json:"checkTimeoutSeconds,omitempty"`

	// Checks is an optional allowlist of check names to run. When empty, all checks run.
	// Checks not in this list are recorded with Result=Skipped.
	// +kubebuilder:validation:Optional
	Checks []string `json:"checks,omitempty"`
}

// CheckExecutionState describes whether a check was able to run.
// +kubebuilder:validation:Enum={"Completed","Error","Skipped"}
type CheckExecutionState string

const (
	// CheckCompleted means the check ran to completion; see Result for the health verdict.
	CheckCompleted CheckExecutionState = "Completed"
	// CheckError means the check could not run due to an infrastructure or transient error.
	CheckError CheckExecutionState = "Error"
	// CheckSkipped means the check was excluded by the Checks allowlist in the spec.
	CheckSkipped CheckExecutionState = "Skipped"
)

// CheckResult is the top-level health verdict for a preflight check.
// +kubebuilder:validation:Enum={"Passed","Failed","Error","Skipped","Unknown"}
type CheckResult string

const (
	// CheckPassed means the cluster is healthy for this check dimension.
	CheckPassed CheckResult = "Passed"
	// CheckFailed means the cluster has a health issue that may block an upgrade.
	CheckFailed CheckResult = "Failed"
	// CheckError means the check could not determine cluster health (infrastructure/transient failure).
	CheckResultError CheckResult = "Error"
	// CheckResultSkipped means the check was not run (excluded by the Checks allowlist).
	CheckResultSkipped CheckResult = "Skipped"
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
