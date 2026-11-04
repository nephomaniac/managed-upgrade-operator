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
	// may run before it is cancelled and recorded as State=Error.
	// Defaults to 30 seconds.
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=30
	CheckTimeoutSeconds int32 `json:"checkTimeoutSeconds,omitempty"`

	// Checks is an optional allowlist of check names to run. When empty, all checks run.
	// Checks not in this list are recorded with State=Skipped.
	// Valid names: ClusterVersion, CriticalAlerts, ClusterOperators,
	//              ManuallyCordonedNodes, NodeUnschedulableTaints, PodDisruptionBudgets,
	//              UpgradeableCondition, PullSecretValid, AdminAcks
	// +kubebuilder:validation:Optional
	Checks []string `json:"checks,omitempty"`
}

// CheckExecutionState describes whether a check was able to run.
// +kubebuilder:validation:Enum={"Completed","Error","Skipped"}
type CheckExecutionState string

const (
	// CheckCompleted means the check ran to completion; see Result for findings.
	CheckCompleted CheckExecutionState = "Completed"
	// CheckError means the check could not run due to an infrastructure or transient error.
	CheckError CheckExecutionState = "Error"
	// CheckSkipped means the check was excluded by the Checks allowlist in the spec.
	CheckSkipped CheckExecutionState = "Skipped"
)

// CheckResult is the health verdict from running a preflight check.
// +kubebuilder:validation:Enum={"Passed","Failed","Unknown"}
type CheckResult string

const (
	// CheckPassed means the cluster is healthy for this check dimension.
	CheckPassed CheckResult = "Passed"
	// CheckFailed means the cluster has issues that should block upgrade.
	CheckFailed CheckResult = "Failed"
	// CheckUnknown means we could not determine cluster health (check errored or was skipped).
	CheckUnknown CheckResult = "Unknown"
)

// CheckExecution describes the outcome of running a preflight check.
type CheckExecution struct {
	// State is the execution outcome: Completed, Error, or Skipped.
	State CheckExecutionState `json:"state"`
	// Result is the health verdict: Passed, Failed, or Unknown.
	Result CheckResult `json:"result"`
	// Message is a human-readable summary (typically present when Result=Failed).
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`
	// Details contains per-item findings (e.g. individual alert names, node names).
	// Structured for tooling; parse with jq/yq via .execution.details[].
	// +kubebuilder:validation:Optional
	Details []string `json:"details,omitempty"`
	// Error contains the reason a check could not run (State=Error only).
	// This is an infrastructure/transient failure, not a cluster health finding.
	// +kubebuilder:validation:Optional
	Error string `json:"error,omitempty"`
}

// PreflightCheckResult holds the execution outcome and finding for a single preflight check.
type PreflightCheckResult struct {
	// Name identifies the check (e.g. "CriticalAlerts", "ClusterOperators").
	Name string `json:"name"`
	// Description explains what this check validates, why it matters for upgrades,
	// and what cluster resources it examines. Educational context for cluster administrators.
	// +kubebuilder:validation:Optional
	Description string `json:"description,omitempty"`
	// Execution contains the state, result, and details from running the check.
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
// Checks are currently platform-agnostic; type- and version-aware checks are planned.
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
