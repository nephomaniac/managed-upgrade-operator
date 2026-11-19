# Proposal: Structured Findings for Upgrade Diagnostics Checks

## Problem Statement

Current check interface limitations:
1. **Mixed ownership**: Checks can find both customer and Red Hat issues but can only return one owner
2. **Unstructured findings**: All data flattened to strings, limiting UI capabilities
3. **No per-finding actions**: Can't provide specific remediation for each finding
4. **No resource references**: UI can't deep-link to problematic resources
5. **Loss of structured data**: Check has rich data but must flatten it to strings

## Proposed Solution

### Phase 1: Add Structured Findings (Opt-In)

#### API Changes

**Add to `api/v1alpha1/upgradediagscheck_types.go`:**

```go
// StructuredFinding provides rich, structured information about a specific issue.
// When present, UIs should prefer this over parsing the string Findings.
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

	// DocumentationURL provides a link to relevant docs/SOPs.
	// +kubebuilder:validation:Optional
	DocumentationURL string `json:"documentationURL,omitempty"`

	// Metadata provides additional structured context as key-value pairs.
	// Examples: thresholds, actual values, related metrics.
	// +kubebuilder:validation:Optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ResourceReference identifies a specific Kubernetes resource.
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

// FindingSeverity indicates the priority/impact of a finding.
type FindingSeverity string

const (
	// FindingSeverityCritical - Upgrade will likely fail or cause outage
	FindingSeverityCritical FindingSeverity = "critical"
	// FindingSeverityHigh - Significant risk, should be addressed before upgrade
	FindingSeverityHigh FindingSeverity = "high"
	// FindingSeverityMedium - Moderate risk, monitor during upgrade
	FindingSeverityMedium FindingSeverity = "medium"
	// FindingSeverityLow - Minor issue, informational
	FindingSeverityLow FindingSeverity = "low"
)

// Update DiagsCheckResult:
type DiagsCheckResult struct {
	// ... existing fields ...

	// StructuredFindings provides rich, machine-readable finding details.
	// When present, UIs should prefer this over parsing string Findings.
	// +kubebuilder:validation:Optional
	StructuredFindings []StructuredFinding `json:"structuredFindings,omitempty"`
}
```

#### Controller Changes

**Add to `controllers/upgradediags/check_interface.go`:**

```go
// CheckOutput is returned by ReadinessCheck.Run.
type CheckOutput struct {
	Result   upgradev1alpha1.CheckResult
	Summary  string

	// Simple approach (backwards compatible)
	Findings []string
	Error    string
	Owner    upgradev1alpha1.CheckOwner
	Action   string

	// Structured approach (opt-in, recommended for new checks)
	StructuredFindings []upgradev1alpha1.StructuredFinding

	// Check-level metadata
	Metadata map[string]string
}
```

**Update `controllers/upgradediags/check_helpers.go`:**

```go
func runCheck(check DiagnosticsCheck, ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) (result upgradev1alpha1.DiagsCheckResult) {
	// ... existing logic ...

	output := check.Run(checkCtx, deps, logger, timeout)

	// ... existing result building ...

	// If check provided structured findings, include them
	if len(output.StructuredFindings) > 0 {
		r.StructuredFindings = output.StructuredFindings

		// Also populate string Findings for backwards compatibility
		if len(r.Execution.Findings) == 0 {
			r.Execution.Findings = make([]string, len(output.StructuredFindings))
			for i, sf := range output.StructuredFindings {
				r.Execution.Findings[i] = sf.Message
			}
		}
	}

	// ... return result ...
}
```

### Phase 2: Helper Functions for Check Authors

**Add to `controllers/upgradediags/finding_builders.go` (new file):**

```go
package upgradediags

import upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"

// FindingBuilder provides a fluent API for creating structured findings.
type FindingBuilder struct {
	finding upgradev1alpha1.StructuredFinding
}

// NewFinding creates a finding builder with a required message.
func NewFinding(message string) *FindingBuilder {
	return &FindingBuilder{
		finding: upgradev1alpha1.StructuredFinding{
			Message: message,
		},
	}
}

// WithSeverity sets the finding severity.
func (b *FindingBuilder) WithSeverity(severity upgradev1alpha1.FindingSeverity) *FindingBuilder {
	b.finding.Severity = severity
	return b
}

// WithOwner sets who is responsible (auto-populates ownerLabel).
func (b *FindingBuilder) WithOwner(owner upgradev1alpha1.CheckOwner) *FindingBuilder {
	b.finding.Owner = owner
	switch owner {
	case upgradev1alpha1.OwnerCustomer:
		b.finding.OwnerLabel = "Admin action required"
	case upgradev1alpha1.OwnerServiceTeam:
		b.finding.OwnerLabel = "Red Hat action required"
	}
	return b
}

// WithAction sets the remediation step.
func (b *FindingBuilder) WithAction(action string) *FindingBuilder {
	b.finding.Action = action
	return b
}

// WithResource sets the related Kubernetes resource.
func (b *FindingBuilder) WithResource(kind, namespace, name string) *FindingBuilder {
	b.finding.Resource = &upgradev1alpha1.ResourceReference{
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
	}
	return b
}

// WithPDB is a convenience method for PDB resources.
func (b *FindingBuilder) WithPDB(namespace, name string) *FindingBuilder {
	return b.WithResource("PodDisruptionBudget", namespace, name)
}

// WithNode is a convenience method for Node resources.
func (b *FindingBuilder) WithNode(name string) *FindingBuilder {
	return b.WithResource("Node", "", name)
}

// WithPod is a convenience method for Pod resources.
func (b *FindingBuilder) WithPod(namespace, name string) *FindingBuilder {
	return b.WithResource("Pod", namespace, name)
}

// WithDocumentation adds a documentation URL.
func (b *FindingBuilder) WithDocumentation(url string) *FindingBuilder {
	b.finding.DocumentationURL = url
	return b
}

// WithMetadata adds a metadata key-value pair.
func (b *FindingBuilder) WithMetadata(key, value string) *FindingBuilder {
	if b.finding.Metadata == nil {
		b.finding.Metadata = make(map[string]string)
	}
	b.finding.Metadata[key] = value
	return b
}

// Build returns the constructed finding.
func (b *FindingBuilder) Build() upgradev1alpha1.StructuredFinding {
	return b.finding
}
```

### Phase 3: Example Migration

**Before (check_pdb.go current approach):**

```go
msg := fmt.Sprintf("%s [owner=%s]: BLOCKING DRAIN — disruptionsAllowed=0 currentHealthy=%d desiredHealthy=%d %s",
    key, owner, currentHealthy, desiredHealthy, uhepStr)
findings = append(findings, msg)
```

**After (using structured findings):**

```go
finding := NewFinding(fmt.Sprintf("%s: BLOCKING DRAIN — disruptionsAllowed=0, currentHealthy=%d, desiredHealthy=%d",
    key, currentHealthy, desiredHealthy)).
    WithSeverity(upgradev1alpha1.FindingSeverityCritical).
    WithOwner(ownerCode).  // auto-populates ownerLabel
    WithPDB(pdb.Namespace, pdb.Name).
    WithAction("Coordinate with application owner to scale up pods or temporarily relax the PDB.").
    WithMetadata("disruptionsAllowed", "0").
    WithMetadata("currentHealthy", fmt.Sprintf("%d", currentHealthy)).
    WithMetadata("desiredHealthy", fmt.Sprintf("%d", desiredHealthy)).
    Build()

structuredFindings = append(structuredFindings, finding)
```

**Return both for backwards compatibility:**

```go
return CheckOutput{
    Result:             upgradev1alpha1.CheckResultWarning,
    Summary:            fmt.Sprintf("%d PDB(s) will stall node drain", len(blocking)),
    Findings:           simpleFindings,  // string array for old clients
    StructuredFindings: structuredFindings,  // rich data for new UIs
    Owner:              dominantOwner,
}
```

### Phase 4: Console Plugin Enhancement

**Update `consoleplugin/src/types.ts`:**

```typescript
export interface ResourceReference {
  kind: string;
  namespace?: string;
  name: string;
  apiVersion?: string;
}

export interface StructuredFinding {
  message: string;
  severity?: 'critical' | 'high' | 'medium' | 'low';
  owner?: CheckOwner;
  ownerLabel?: string;
  action?: string;
  resource?: ResourceReference;
  documentationURL?: string;
  metadata?: Record<string, string>;
}

export interface DiagsCheckResult {
  // ... existing fields ...
  structuredFindings?: StructuredFinding[];
}
```

**Console UI can now:**
1. Group findings by owner (customer vs Red Hat)
2. Filter by severity
3. Deep-link to resources (click PDB name → navigate to PDB detail)
4. Show per-finding actions
5. Display metadata in expandable sections
6. Sort/filter by any field

## Benefits

### For Check Authors
- ✅ **Easy to use**: Fluent builder API
- ✅ **Type-safe**: Compile-time validation
- ✅ **Flexible**: Use simple strings or rich structures
- ✅ **Backwards compatible**: Existing checks continue to work

### For UI Developers
- ✅ **Rich data**: No string parsing needed
- ✅ **Deep linking**: Navigate to problem resources
- ✅ **Smart filtering**: By owner, severity, resource type
- ✅ **Better UX**: Per-finding actions and docs

### For Users
- ✅ **Clear ownership**: Each finding shows who needs to act
- ✅ **Prioritization**: Severity guides triage
- ✅ **Quick navigation**: Click to view problem resource
- ✅ **Better guidance**: Per-finding remediation steps

## Migration Strategy

1. **Phase 1**: Add API types (this PR)
2. **Phase 2**: Add builder helpers
3. **Phase 3**: Migrate 1-2 checks as examples (PDB, Draining Nodes)
4. **Phase 4**: Update console plugin to use structured data
5. **Phase 5**: Gradually migrate remaining checks
6. **Maintain**: String findings remain for backwards compatibility

## Backwards Compatibility

- ✅ Existing checks work unchanged
- ✅ String `findings` array still populated
- ✅ Old UIs/CLIs see string findings
- ✅ New UIs prefer structured findings when available
- ✅ No breaking changes to CRD (all new fields optional)
