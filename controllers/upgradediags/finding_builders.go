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

// WithOwner sets who is responsible and auto-populates the user-facing ownerLabel.
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

// WithAction sets the specific remediation step for this finding.
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

// WithResourceAPIVersion sets the related resource with an explicit API version.
func (b *FindingBuilder) WithResourceAPIVersion(kind, namespace, name, apiVersion string) *FindingBuilder {
	b.finding.Resource = &upgradev1alpha1.ResourceReference{
		Kind:       kind,
		Namespace:  namespace,
		Name:       name,
		APIVersion: apiVersion,
	}
	return b
}

// WithPDB is a convenience method for PodDisruptionBudget resources.
func (b *FindingBuilder) WithPDB(namespace, name string) *FindingBuilder {
	return b.WithResourceAPIVersion("PodDisruptionBudget", namespace, name, "policy/v1")
}

// WithNode is a convenience method for Node resources.
func (b *FindingBuilder) WithNode(name string) *FindingBuilder {
	return b.WithResource("Node", "", name)
}

// WithPod is a convenience method for Pod resources.
func (b *FindingBuilder) WithPod(namespace, name string) *FindingBuilder {
	return b.WithResource("Pod", namespace, name)
}

// WithDocumentation adds a documentation or SOP URL.
func (b *FindingBuilder) WithDocumentation(url string) *FindingBuilder {
	b.finding.DocumentationURL = url
	return b
}

// WithMetadata adds a metadata key-value pair for additional context.
func (b *FindingBuilder) WithMetadata(key, value string) *FindingBuilder {
	if b.finding.Metadata == nil {
		b.finding.Metadata = make(map[string]string)
	}
	b.finding.Metadata[key] = value
	return b
}

// Build returns the constructed StructuredFinding.
func (b *FindingBuilder) Build() upgradev1alpha1.StructuredFinding {
	return b.finding
}
