package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpgradeDiagsCheckSpec defines the target upgrade to run diagnostics for.
// It embeds UpgradeConfigSpec so that all UpgradeConfig fields (type, desired version,
// PDBForceDrainTimeout, etc.) are inherited automatically.
type UpgradeDiagsCheckSpec struct {
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

// CheckResult is the top-level health verdict for a diagnostics check.
// +kubebuilder:validation:Enum={"Passed","Warning","Failed","Error","Skipped","TimedOut","Unknown"}
type CheckResult string

const (
	// CheckPassed means the cluster is healthy for this check dimension.
	CheckPassed CheckResult = "Passed"
	// CheckResultWarning means the check completed and the cluster can proceed, but the consumer
	// should be aware of a non-blocking condition.
	CheckResultWarning CheckResult = "Warning"
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

// CheckExecution describes the runtime execution of a single diagnostics check.
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
	// +kubebuilder:validation:Optional
	Summary string `json:"summary,omitempty"`

	// Findings contains detailed per-item observations from the check.
	// +kubebuilder:validation:Optional
	Findings []string `json:"findings,omitempty"`

	// Error contains the reason a check could not run (State=Error only).
	// +kubebuilder:validation:Optional
	Error string `json:"error,omitempty"`
}

// FindingSeverity indicates the priority/impact of a structured finding.
// +kubebuilder:validation:Enum={"critical","high","medium","low"}
type FindingSeverity string

const (
	// FindingSeverityCritical means the upgrade will likely fail or cause an outage.
	FindingSeverityCritical FindingSeverity = "critical"
	// FindingSeverityHigh means significant risk; should be addressed before upgrade.
	FindingSeverityHigh FindingSeverity = "high"
	// FindingSeverityMedium means moderate risk; monitor during upgrade.
	FindingSeverityMedium FindingSeverity = "medium"
	// FindingSeverityLow means minor issue; informational only.
	FindingSeverityLow FindingSeverity = "low"
)

// ResourceReference identifies a specific Kubernetes resource related to a finding.
type ResourceReference struct {
	// Kind is the Kubernetes resource kind (e.g., "PodDisruptionBudget").
	Kind string `json:"kind"`

	// Namespace is the resource namespace (empty for cluster-scoped resources).
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the resource name.
	Name string `json:"name"`

	// APIVersion is the resource API version (e.g., "policy/v1").
	// +kubebuilder:validation:Optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// StructuredFinding provides rich, structured information about a specific issue.
// When present, UIs should prefer this over parsing the string Findings list.
type StructuredFinding struct {
	// Message is the human-readable description of this finding (required).
	Message string `json:"message"`

	// Severity indicates the priority/impact of this specific finding.
	// +kubebuilder:validation:Optional
	Severity FindingSeverity `json:"severity,omitempty"`

	// Owner identifies who is responsible for resolving THIS finding.
	// Overrides the check-level Owner when present.
	// +kubebuilder:validation:Optional
	Owner CheckOwner `json:"owner,omitempty"`

	// OwnerLabel is the user-facing text for this finding's owner.
	// +kubebuilder:validation:Optional
	OwnerLabel string `json:"ownerLabel,omitempty"`

	// Action is the specific remediation step for THIS finding.
	// +kubebuilder:validation:Optional
	Action string `json:"action,omitempty"`

	// Resource identifies the specific Kubernetes resource this finding relates to.
	// +kubebuilder:validation:Optional
	Resource *ResourceReference `json:"resource,omitempty"`

	// DocumentationURL provides a link to relevant docs or SOPs.
	// +kubebuilder:validation:Optional
	DocumentationURL string `json:"documentationURL,omitempty"`

	// Metadata provides additional structured context as key-value pairs.
	// +kubebuilder:validation:Optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// DiagsCheckResult holds the result and execution details for a single diagnostics check.
type DiagsCheckResult struct {
	// Name identifies the check (e.g. "CriticalAlerts", "ClusterOperators").
	Name string `json:"name"`

	// Description is a human-readable explanation of what this check validates and why it matters.
	// +kubebuilder:validation:Optional
	Description string `json:"description,omitempty"`

	// Result is the health verdict: Passed, Failed, Error, Skipped, or Unknown.
	Result CheckResult `json:"result"`

	// Owner identifies who is responsible for resolving this check when it fails or errors.
	// +kubebuilder:validation:Optional
	Owner CheckOwner `json:"owner,omitempty"`

	// Action is the recommended next step when Result is Failed or Error.
	// +kubebuilder:validation:Optional
	Action string `json:"action,omitempty"`

	// TimeoutSeconds is the timeout that was applied to this check.
	TimeoutSeconds int32 `json:"timeoutSeconds"`

	// Execution contains runtime information about how the check ran.
	Execution CheckExecution `json:"execution"`

	// StructuredFindings provides rich, machine-readable finding details.
	// When present, UIs should prefer this over parsing string Findings.
	// +kubebuilder:validation:Optional
	StructuredFindings []StructuredFinding `json:"structuredFindings,omitempty"`
}

const (
	// DiagsCompleteCondition is True when all checks have finished running.
	// The Reason field carries the outcome: Passed, Warning, or Failed.
	// Use with: oc wait --for=condition=Complete --timeout=5m udc/<name>
	DiagsCompleteCondition = "Complete"
)

// UpgradeDiagsCheckStatus reports the outcome of all diagnostics checks.
type UpgradeDiagsCheckStatus struct {
	// ClusterInfraName is the cluster's infrastructure name (Infrastructure.status.infrastructureName).
	// Populated at run time so results are unambiguously tied to the cluster that produced them.
	// +kubebuilder:validation:Optional
	ClusterInfraName string `json:"clusterInfraName,omitempty"`
	// APIServerURL is the cluster's API server URL (Infrastructure.status.apiServerURL).
	// +kubebuilder:validation:Optional
	APIServerURL string `json:"apiServerURL,omitempty"`

	// +kubebuilder:validation:Optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +kubebuilder:validation:Optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// OverallResult is the top-level verdict once all checks are complete.
	// +kubebuilder:validation:Optional
	OverallResult CheckResult `json:"overallResult,omitempty"`

	// +kubebuilder:validation:Optional
	ChecksPassed int `json:"checksPassed,omitempty"`
	// +kubebuilder:validation:Optional
	ChecksWarned int `json:"checksWarned,omitempty"`
	// +kubebuilder:validation:Optional
	ChecksFailed int `json:"checksFailed,omitempty"`
	// +kubebuilder:validation:Optional
	ChecksErrored int `json:"checksErrored,omitempty"`

	// Conditions exposes the run state as standard Kubernetes conditions.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +kubebuilder:validation:Optional
	Checks []DiagsCheckResult `json:"checks,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=upgradediagschecks,scope=Namespaced,shortName=udc
// +kubebuilder:printcolumn:name="version",type="string",JSONPath=".spec.desired.version"
// +kubebuilder:printcolumn:name="channel",type="string",JSONPath=".spec.desired.channel"
// +kubebuilder:printcolumn:name="type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="status",type="string",JSONPath=".status.overallResult",description="Overall check result"
// +kubebuilder:printcolumn:name="passed",type="integer",JSONPath=".status.checksPassed"
// +kubebuilder:printcolumn:name="warned",type="integer",JSONPath=".status.checksWarned"
// +kubebuilder:printcolumn:name="failed",type="integer",JSONPath=".status.checksFailed"
// +kubebuilder:printcolumn:name="completed",type="string",JSONPath=".status.completedAt"

// UpgradeDiagsCheck is the Schema for on-demand cluster upgrade diagnostics checks.
// Creating one runs pre-upgrade health checks without scheduling or starting an upgrade.
type UpgradeDiagsCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpgradeDiagsCheckSpec   `json:"spec,omitempty"`
	Status UpgradeDiagsCheckStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UpgradeDiagsCheckList contains a list of UpgradeDiagsCheck.
type UpgradeDiagsCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpgradeDiagsCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UpgradeDiagsCheck{}, &UpgradeDiagsCheckList{})
}
