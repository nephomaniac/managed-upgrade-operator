package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	descNodeHealth         = "Checks for manually cordoned worker nodes and nodes with memory, disk, or PID pressure taints that would block scheduling, using the same checks MUO runs before starting an upgrade."
	descNodeHealthExtended = "In-depth node health analysis. Shows all worker nodes that are cordoned or have memory, disk, or PID pressure conditions that could block workload scheduling."
)

type manuallyCordonedNodesCheck struct{}

func (c *manuallyCordonedNodesCheck) Name() string        { return "ManuallyCordonedNodes" }
func (c *manuallyCordonedNodesCheck) Description() string { return descNodeHealth }
func (c *manuallyCordonedNodesCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *manuallyCordonedNodesCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	metricsClient, err := deps.MetricsClientBuilder.NewClient(deps.Client)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to create metrics client: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node status cannot be determined.",
		}
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.ManuallyCordonedNodes)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	cordonedNodes, err := upgraders.ManuallyCordonedNodes(metricsClient, deps.Machinery, deps.Client, fakeUC, logger, "")
	if err != nil && len(cordonedNodes) == 0 {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to check cordoned nodes: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node status cannot be determined.",
		}
	}
	if len(cordonedNodes) > 0 {
		for _, node := range cordonedNodes {
			findings = append(findings, fmt.Sprintf("cordoned worker node: %s", node))
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Upgrade validation failed: %d manually cordoned worker node(s)", len(cordonedNodes)),
			Findings: findings,
			Action:   "Uncordon the listed nodes before upgrading: `oc adm uncordon <node-name>`",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Upgrade validation passed: no manually cordoned nodes",
		Findings: findings,
	}
}

type manuallyCordonedNodesExtendedCheck struct{}

func (c *manuallyCordonedNodesExtendedCheck) Name() string        { return "ManuallyCordonedNodesExtended" }
func (c *manuallyCordonedNodesExtendedCheck) Description() string { return descNodeHealthExtended }
func (c *manuallyCordonedNodesExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *manuallyCordonedNodesExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	nodes := &corev1.NodeList{}
	if err := deps.Client.List(ctx, nodes, &client.ListOptions{
		Raw: &metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker, !node-role.kubernetes.io/infra"},
	}); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list worker nodes: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node status cannot be listed.",
		}
	}

	var cordoned, findings []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		cr := deps.Machinery.IsNodeCordoned(node)
		if cr.IsCordoned && !deps.Machinery.IsNodeUpgrading(node) {
			cordoned = append(cordoned, node.Name)
			findings = append(findings, fmt.Sprintf("%s: manually cordoned", node.Name))
		} else {
			findings = append(findings, fmt.Sprintf("%s: schedulable", node.Name))
		}
	}
	if len(cordoned) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d manually cordoned node(s) of %d worker node(s)", len(cordoned), len(nodes.Items)),
			Findings: findings,
			Action:   "Uncordon the listed nodes before upgrading: `oc adm uncordon <node-name>`",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("All %d worker node(s) are schedulable", len(nodes.Items)),
		Findings: findings,
	}
}

type nodeTaintsCheck struct{}

func (c *nodeTaintsCheck) Name() string        { return "NodeUnschedulableTaints" }
func (c *nodeTaintsCheck) Description() string { return descNodeHealth }
func (c *nodeTaintsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *nodeTaintsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	metricsClient, err := deps.MetricsClientBuilder.NewClient(deps.Client)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to create metrics client: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node taint status cannot be determined.",
		}
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.NodeUnschedulableTaints)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	taintedNodes, err := upgraders.NodeUnschedulableTaints(metricsClient, deps.Machinery, deps.Client, fakeUC, logger, "")
	if err != nil && len(taintedNodes) == 0 {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to check node taints: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node taint status cannot be determined.",
		}
	}
	if len(taintedNodes) > 0 {
		for _, node := range taintedNodes {
			findings = append(findings, fmt.Sprintf("node with pressure taint: %s", node))
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Upgrade validation failed: %d node(s) with pressure taints", len(taintedNodes)),
			Findings: findings,
			Action:   "Remove or adjust the unschedulable taints on the listed nodes before upgrading.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Upgrade validation passed: no pressure taints detected",
		Findings: findings,
	}
}

type nodeTaintsExtendedCheck struct{}

func (c *nodeTaintsExtendedCheck) Name() string        { return "NodeUnschedulableTaintsExtended" }
func (c *nodeTaintsExtendedCheck) Description() string { return descNodeHealthExtended }
func (c *nodeTaintsExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *nodeTaintsExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	nodes := &corev1.NodeList{}
	if err := deps.Client.List(ctx, nodes); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list nodes: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node taint status cannot be listed.",
		}
	}
	var affected []string
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if deps.Machinery.HasMemoryPressure(node) || deps.Machinery.HasDiskPressure(node) || deps.Machinery.HasPidPressure(node) {
			affected = append(affected, node.Name)
		}
	}
	if len(affected) > 0 {
		findings := []string{fmt.Sprintf("Total nodes checked: %d", len(nodes.Items))}
		for _, name := range affected {
			findings = append(findings, fmt.Sprintf("%s: unschedulable pressure taint", name))
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d node(s) with pressure taints of %d total", len(affected), len(nodes.Items)),
			Findings: findings,
			Action:   "Remove or adjust the unschedulable taints on the listed nodes before upgrading.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("All %d node(s) checked: no pressure taints", len(nodes.Items)),
		Findings: []string{fmt.Sprintf("Checked %d node(s) for memory, disk, and PID pressure — all healthy", len(nodes.Items))},
	}
}
