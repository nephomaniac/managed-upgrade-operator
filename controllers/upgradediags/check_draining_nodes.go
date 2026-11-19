package upgradediags

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const descStuckDrainingNodes = "Finds nodes that are SchedulingDisabled (cordoned) and still have non-DaemonSet pods " +
	"running on them, indicating a drain that is in progress or stuck. For each pod remaining on a draining node, " +
	"attributes a root cause: PDB blocking eviction (which PDB, disruptionsAllowed, currentHealthy, " +
	"unhealthyPodEvictionPolicy), finalizer preventing pod deletion, long-running graceful termination, " +
	"or eviction still pending. Correctly identifies that --disable-eviction (not --force) is required to " +
	"bypass a PDB-blocked drain. Source: MCS-251/253 (UpgradeNodeDrainFailedSRE CAD investigation gap)."

type stuckDrainingNodesCheck struct{}

func (c *stuckDrainingNodesCheck) Name() string                 { return "StuckDrainingNodes" }
func (c *stuckDrainingNodesCheck) Description() string           { return descStuckDrainingNodes }
func (c *stuckDrainingNodesCheck) DefaultTimeout() time.Duration { return 90 * time.Second }
func (c *stuckDrainingNodesCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *stuckDrainingNodesCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	nodeList := &corev1.NodeList{}
	if err := deps.Client.List(ctx, nodeList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list nodes: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if nodes cannot be listed.",
		}
	}

	var drainingNodes []corev1.Node
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable {
			drainingNodes = append(drainingNodes, node)
		}
	}

	if len(drainingNodes) == 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckPassed,
			Summary:  fmt.Sprintf("No nodes are cordoned or draining (checked %d nodes)", len(nodeList.Items)),
			Findings: []string{fmt.Sprintf("Checked %d nodes: none are SchedulingDisabled", len(nodeList.Items))},
		}
	}

	podList := &corev1.PodList{}
	if err := deps.Client.List(ctx, podList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list pods: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if pods cannot be listed.",
		}
	}

	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := deps.Client.List(ctx, pdbList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list PodDisruptionBudgets: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if PodDisruptionBudgets cannot be listed.",
		}
	}

	// Index non-DaemonSet, non-mirror pods by node name.
	podsByNode := make(map[string][]corev1.Pod)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName == "" || isDrainIgnoredPod(*pod) {
			continue
		}
		podsByNode[pod.Spec.NodeName] = append(podsByNode[pod.Spec.NodeName], *pod)
	}

	now := metav1.Now()
	nsCache := newNamespacePlatformCache(deps.Client)

	var stuckNodeNames []string
	var findings []string
	hasServiceTeamBlocker := false

	for _, node := range drainingNodes {
		pods := podsByNode[node.Name]

		if len(pods) == 0 {
			findings = append(findings, fmt.Sprintf("node/%s: cordoned, no non-DaemonSet pods remaining (drain complete or not yet started)", node.Name))
			continue
		}

		var podLines []string
		nodeStuck := false

		for _, pod := range pods {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			isPlatform := nsCache.IsPlatform(ctx, pod.Namespace)

			switch {
			case pod.DeletionTimestamp != nil && len(pod.Finalizers) > 0:
				// Finalizer is holding the pod — a controller hasn't released it.
				podLines = append(podLines, fmt.Sprintf(
					"  %s: Terminating %s — finalizers=[%s] (controller not releasing pod)",
					key, now.Sub(pod.DeletionTimestamp.Time).Round(time.Second),
					strings.Join(pod.Finalizers, ",")))
				nodeStuck = true
				if isPlatform {
					hasServiceTeamBlocker = true
				}

			case pod.DeletionTimestamp != nil && now.Sub(pod.DeletionTimestamp.Time) >= stuckTerminatingThreshold:
				// No finalizer but container hasn't exited after the threshold.
				podLines = append(podLines, fmt.Sprintf(
					"  %s: Terminating %s — no finalizers, container not exiting (check SIGTERM handler / grace period)",
					key, now.Sub(pod.DeletionTimestamp.Time).Round(time.Second)))
				nodeStuck = true
				if isPlatform {
					hasServiceTeamBlocker = true
				}

			case pod.DeletionTimestamp != nil:
				// Graceful termination within normal window.
				podLines = append(podLines, fmt.Sprintf(
					"  %s: Terminating %s — graceful shutdown in progress",
					key, now.Sub(pod.DeletionTimestamp.Time).Round(time.Second)))

			default:
				// Pod not yet terminating — check for a PDB blocking eviction.
				if blocking := findBlockingPDBForPod(pod, pdbList.Items); blocking != nil {
					uhepStr := describeUHEP(blocking.Spec.UnhealthyPodEvictionPolicy)
					podLines = append(podLines, fmt.Sprintf(
						"  %s: running — PDB %s/%s blocks eviction (disruptionsAllowed=%d currentHealthy=%d %s)",
						key, blocking.Namespace, blocking.Name,
						blocking.Status.DisruptionsAllowed, blocking.Status.CurrentHealthy, uhepStr))
					nodeStuck = true
					if isPlatform {
						hasServiceTeamBlocker = true
					}
				} else {
					podLines = append(podLines, fmt.Sprintf("  %s: running — eviction pending (no blocking PDB found)", key))
				}
			}
		}

		status := "draining"
		if nodeStuck {
			status = "STUCK"
			stuckNodeNames = append(stuckNodeNames, "node/"+node.Name)
		}
		findings = append(findings, fmt.Sprintf("node/%s: %s — %d non-DaemonSet pod(s) remaining", node.Name, status, len(pods)))
		findings = append(findings, podLines...)
	}

	if len(stuckNodeNames) > 0 {
		owner := upgradev1alpha1.OwnerCustomer
		if hasServiceTeamBlocker {
			owner = upgradev1alpha1.OwnerServiceTeam
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d node(s) stuck draining: %s", len(stuckNodeNames), strings.Join(stuckNodeNames, ", ")),
			Findings: findings,
			Owner:    owner,
			Action: "Review per-pod root causes above. " +
				"PDB-blocked: use 'oc adm drain <node> --disable-eviction' to bypass PDB enforcement by switching to plain pod delete " +
				"(--force does NOT bypass PDBs — it is enforced server-side; this is a known SOP error per MCS-251). " +
				"Only bypass after confirming currentHealthy=0 or after cluster admin coordination (see PodDisruptionBudgetsDrainSafety findings). " +
				"Finalizer-stuck: identify and remediate the responsible controller (storage driver, network plugin, operator). " +
				"Long-terminating: 'oc delete pod <name> -n <ns> --grace-period=0 --force' after confirming it is safe.",
		}
	}

	return CheckOutput{
		Result:   upgradev1alpha1.CheckResultWarning,
		Summary:  fmt.Sprintf("%d node(s) are cordoned and draining — no pods appear stuck yet", len(drainingNodes)),
		Findings: findings,
		Action:   "Monitor drain progress. Re-run this check to see updated per-pod root cause attribution if pods do not terminate within the expected window.",
	}
}

// isDrainIgnoredPod returns true for pods that oc adm drain skips by default:
// DaemonSet-owned pods and kubelet mirror pods.
func isDrainIgnoredPod(pod corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	_, isMirror := pod.Annotations["kubernetes.io/config.mirror"]
	return isMirror
}

// findBlockingPDBForPod returns the first PDB in the same namespace whose selector
// matches the pod and whose status.disruptionsAllowed == 0, or nil if none.
func findBlockingPDBForPod(pod corev1.Pod, pdbs []policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	for i := range pdbs {
		pdb := &pdbs[i]
		if pdb.Namespace != pod.Namespace || pdb.Status.DisruptionsAllowed > 0 || pdb.Spec.Selector == nil {
			continue
		}
		if podMatchesPDBSelector(pod, *pdb) {
			return pdb
		}
	}
	return nil
}

// describeUHEP returns a compact string describing the unhealthyPodEvictionPolicy
// value and its upgrade-drain implication (Gap B — MCS-254).
func describeUHEP(policy *policyv1.UnhealthyPodEvictionPolicyType) string {
	if policy == nil || *policy == policyv1.IfHealthyBudget {
		return "unhealthyPodEvictionPolicy=IfHealthyBudget (default: unhealthy pods also blocked; set AlwaysAllow to self-heal)"
	}
	if *policy == policyv1.AlwaysAllow {
		return "unhealthyPodEvictionPolicy=AlwaysAllow (unhealthy pods can be evicted; PDB will self-resolve when pods degrade)"
	}
	return fmt.Sprintf("unhealthyPodEvictionPolicy=%s", string(*policy))
}
