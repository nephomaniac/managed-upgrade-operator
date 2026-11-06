package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	descPodDisruptionBudgets = "Checks for PDBs that block node drains, using the same HealthCheckPDB function MUO calls during upgrades. Combines direct PDB inspection with DVO metrics."
	descPDBMasterInfra       = "Identifies PodDisruptionBudgets that could block master or infra node drain during an upgrade. Scans two sources: (1) pods currently running on master/infra nodes, and (2) DaemonSet pod templates that tolerate master/infra taints and will always be present on those nodes. Only flags PDBs that are restrictive (MaxUnavailable=0 or MinAvailable=100%) and are confirmed to cover workloads on those nodes."
)

type podDisruptionBudgetsCheck struct{}

func (c *podDisruptionBudgetsCheck) Name() string        { return "PodDisruptionBudgets" }
func (c *podDisruptionBudgetsCheck) Description() string { return descPodDisruptionBudgets }
func (c *podDisruptionBudgetsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *podDisruptionBudgetsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	if deps.DvoClientBuilder == nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  "DvoClientBuilder is not configured",
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support — the DVO client is missing from the operator configuration.",
		}
	}
	if ctx.Err() != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("context done before check could start: %v", ctx.Err()),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if the PDB check consistently times out.",
		}
	}
	metricsClient, err := deps.MetricsClientBuilder.NewClient(deps.Client)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to build metrics client: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if Prometheus cannot be reached.",
		}
	}
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.HealthCheckPDB)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	_, passed, err := upgraders.HealthCheckPDB(metricsClient, deps.Client, deps.DvoClientBuilder, fakeUC, logger, fakeUC.Spec.Desired.Version)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("PodDisruptionBudget check failed: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if the PDB check fails to execute.",
		}
	}
	if !passed {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  "Upgrade validation failed: blocking PDBs or invalid PDB configuration detected",
			Findings: findings,
			Action:   "Adjust PodDisruptionBudgets in openshift-* namespaces to allow at least one pod to be unavailable during node drains (MaxUnavailable≥1 or MinAvailable<100%).",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Upgrade validation passed: no blocking PDBs",
		Findings: findings,
	}
}

type pdbMasterInfraExtendedCheck struct{}

func (c *pdbMasterInfraExtendedCheck) Name() string { return "PodDisruptionBudgetsMasterInfraExtended" }
func (c *pdbMasterInfraExtendedCheck) Description() string { return descPDBMasterInfra }
func (c *pdbMasterInfraExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *pdbMasterInfraExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	nodeList := &corev1.NodeList{}
	if err := deps.Client.List(ctx, nodeList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list nodes: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if node listing fails.",
		}
	}

	masterInfraNames := map[string]string{}
	for _, node := range nodeList.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			masterInfraNames[node.Name] = "master"
		} else if _, ok := node.Labels["node-role.kubernetes.io/infra"]; ok {
			masterInfraNames[node.Name] = "infra"
		}
	}

	// Source 1: pods currently running on master/infra nodes.
	podList := &corev1.PodList{}
	if err := deps.Client.List(ctx, podList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list pods: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if pod listing fails.",
		}
	}
	masterInfraPods := []corev1.Pod{}
	for _, pod := range podList.Items {
		if _, ok := masterInfraNames[pod.Spec.NodeName]; ok {
			masterInfraPods = append(masterInfraPods, pod)
		}
	}

	// Source 2: DaemonSet pod templates that tolerate master/infra taints.
	// DaemonSets with these tolerations will always place pods on master/infra
	// nodes regardless of whether pods are currently running.
	dsList := &appsv1.DaemonSetList{}
	if err := deps.Client.List(ctx, dsList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list DaemonSets: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if DaemonSet listing fails.",
		}
	}

	pdbList := &policyv1.PodDisruptionBudgetList{}
	if err := deps.Client.List(ctx, pdbList); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list PodDisruptionBudgets: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if PDB listing fails.",
		}
	}

	// Track PDBs already flagged to avoid duplicate findings.
	flagged := map[string]bool{}
	var blocking, findings []string

	for _, pdb := range pdbList.Items {
		key := fmt.Sprintf("%s/%s", pdb.Namespace, pdb.Name)

		// Check running pods on master/infra nodes.
		if covered := pdbCoveredMasterInfraPods(pdb, masterInfraPods, masterInfraNames); len(covered) > 0 {
			flagged[key] = true
			if reason := pdbViolation(pdb); reason != "" {
				detail := fmt.Sprintf("%s: covers %d running pods on master/infra nodes (%s)", key, len(covered), reason)
				blocking = append(blocking, detail)
				findings = append(findings, detail)
			} else {
				findings = append(findings, fmt.Sprintf("%s: ok (%d running pods on master/infra)", key, len(covered)))
			}
			continue
		}

		// Check DaemonSets that target master/infra nodes.
		if ds, matched := pdbMatchesMasterInfraDS(pdb, dsList.Items); matched {
			flagged[key] = true
			if reason := pdbViolation(pdb); reason != "" {
				detail := fmt.Sprintf("%s: covers DaemonSet %s/%s targeting master/infra nodes (%s)", key, ds.Namespace, ds.Name, reason)
				blocking = append(blocking, detail)
				findings = append(findings, detail)
			} else {
				findings = append(findings, fmt.Sprintf("%s: ok (covers DaemonSet %s/%s targeting master/infra)", key, ds.Namespace, ds.Name))
			}
		}
	}

	_ = flagged
	if len(blocking) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Found %d restrictive PDB(s) that could block drain of %d master/infra node(s)", len(blocking), len(masterInfraNames)),
			Findings: findings,
			Action:   "Adjust the listed PodDisruptionBudgets to allow node drains, or move the workloads off master/infra nodes.",
		}
	}
	summary := fmt.Sprintf("Checked PDBs against %d master/infra node(s): none blocking", len(masterInfraNames))
	if len(masterInfraNames) == 0 {
		summary = "No master/infra nodes found"
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  summary,
		Findings: findings,
	}
}

func pdbViolation(pdb policyv1.PodDisruptionBudget) string {
	if pdb.Spec.MaxUnavailable != nil {
		switch pdb.Spec.MaxUnavailable.Type {
		case intstr.Int:
			if pdb.Spec.MaxUnavailable.IntVal == 0 {
				return "MaxUnavailable=0"
			}
		case intstr.String:
			if pdb.Spec.MaxUnavailable.StrVal == "0%" {
				return "MaxUnavailable=0%"
			}
		}
	}
	if pdb.Spec.MinAvailable != nil && pdb.Spec.MinAvailable.Type == intstr.String {
		if pdb.Spec.MinAvailable.StrVal == "100%" {
			return "MinAvailable=100%"
		}
	}
	return ""
}

func pdbCoveredMasterInfraPods(pdb policyv1.PodDisruptionBudget, pods []corev1.Pod, nodeRoles map[string]string) []corev1.Pod {
	if pdb.Spec.Selector == nil {
		return nil
	}
	var covered []corev1.Pod
	for _, pod := range pods {
		if pod.Namespace != pdb.Namespace {
			continue
		}
		if _, onMasterInfra := nodeRoles[pod.Spec.NodeName]; !onMasterInfra {
			continue
		}
		if podMatchesPDBSelector(pod, pdb) {
			covered = append(covered, pod)
		}
	}
	return covered
}

func podMatchesPDBSelector(pod corev1.Pod, pdb policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	for k, v := range pdb.Spec.Selector.MatchLabels {
		if pod.Labels[k] != v {
			return false
		}
	}
	return true
}

// pdbMatchesMasterInfraDS returns the first DaemonSet whose pod template is covered
// by the PDB's selector and whose pod template tolerates master or infra taints.
func pdbMatchesMasterInfraDS(pdb policyv1.PodDisruptionBudget, dsList []appsv1.DaemonSet) (appsv1.DaemonSet, bool) {
	if pdb.Spec.Selector == nil {
		return appsv1.DaemonSet{}, false
	}
	for _, ds := range dsList {
		if ds.Namespace != pdb.Namespace {
			continue
		}
		if !dsToleratsMasterOrInfra(ds) {
			continue
		}
		if dsMatchesPDBSelector(ds, pdb) {
			return ds, true
		}
	}
	return appsv1.DaemonSet{}, false
}

func dsToleratsMasterOrInfra(ds appsv1.DaemonSet) bool {
	for _, t := range ds.Spec.Template.Spec.Tolerations {
		if t.Key == "node-role.kubernetes.io/master" || t.Key == "node-role.kubernetes.io/infra" {
			return true
		}
		// A wildcard toleration (key="", operator=Exists) covers all taints including master/infra.
		if t.Key == "" && t.Operator == corev1.TolerationOpExists {
			return true
		}
	}
	return false
}

func dsMatchesPDBSelector(ds appsv1.DaemonSet, pdb policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}
	podLabels := ds.Spec.Template.Labels
	for k, v := range pdb.Spec.Selector.MatchLabels {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}
