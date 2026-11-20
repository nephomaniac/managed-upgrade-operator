package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const descPausedMCP = "Checks for MachineConfigPools with spec.paused=true. A paused pool will not roll worker nodes during an upgrade — MUO step 12 (AllWorkersUpgraded) waits indefinitely until the pool is unpaused. Source: OHSS Case 3 (worker rollout stalls)."

var mcpCheckGVK = schema.GroupVersionKind{Group: "machineconfiguration.openshift.io", Version: "v1", Kind: "MachineConfigPool"}

type pausedMachineConfigPoolCheck struct{}

func (c *pausedMachineConfigPoolCheck) Name() string                 { return "PausedMachineConfigPool" }
func (c *pausedMachineConfigPoolCheck) Description() string           { return descPausedMCP }
func (c *pausedMachineConfigPoolCheck) DefaultTimeout() time.Duration { return 0 }
func (c *pausedMachineConfigPoolCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *pausedMachineConfigPoolCheck) UpgradePhase() string { return "worker-upgrade" }

func (c *pausedMachineConfigPoolCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	mcpList := &unstructured.UnstructuredList{}
	mcpList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: mcpCheckGVK.Group, Version: mcpCheckGVK.Version, Kind: "MachineConfigPoolList",
	})
	if err := deps.Client.List(ctx, mcpList, &client.ListOptions{}); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list MachineConfigPools: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if MachineConfigPools cannot be listed.",
		}
	}

	var paused []string
	var findings []string
	for _, mcp := range mcpList.Items {
		name := mcp.GetName()
		isPaused, _, _ := unstructured.NestedBool(mcp.Object, "spec", "paused")
		if isPaused {
			paused = append(paused, name)
			findings = append(findings, fmt.Sprintf("%s: paused=true", name))
		} else {
			findings = append(findings, fmt.Sprintf("%s: not paused", name))
		}
	}

	if len(paused) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d MachineConfigPool(s) are paused and will block worker node rollout", len(paused)),
			Findings: findings,
			Action:   "Unpause each pool before upgrading: oc patch mcp <name> --type merge -p '{\"spec\":{\"paused\":false}}'",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("All %d MachineConfigPool(s) are unpaused", len(mcpList.Items)),
		Findings: findings,
	}
}
