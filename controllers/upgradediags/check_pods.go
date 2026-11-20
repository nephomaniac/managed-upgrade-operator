package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	stuckTerminatingThreshold = 5 * time.Minute
	descStuckTerminating      = "Finds pods stuck in Terminating for more than 5 minutes. These pods block node drain during both the extra-worker scale-down (MUO step 13) and the main worker rollout, causing UpgradeNodeDrainFailedSRE alerts. Source: OHSS Case 7 (drain stalls)."
)

type stuckTerminatingPodsCheck struct{}

func (c *stuckTerminatingPodsCheck) Name() string                 { return "StuckTerminatingPods" }
func (c *stuckTerminatingPodsCheck) Description() string           { return descStuckTerminating }
func (c *stuckTerminatingPodsCheck) DefaultTimeout() time.Duration { return 60 * time.Second }
func (c *stuckTerminatingPodsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *stuckTerminatingPodsCheck) UpgradePhase() string { return "worker-upgrade" }

func (c *stuckTerminatingPodsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	pods := &corev1.PodList{}
	if err := deps.Client.List(ctx, pods, &client.ListOptions{}); err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list pods: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if pods cannot be listed.",
		}
	}

	now := metav1.Now()
	var stuck []string
	var findings []string

	nsCache := newNamespacePlatformCache(deps.Client)
	serviceTeamPod := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil {
			continue
		}
		age := now.Sub(pod.DeletionTimestamp.Time)
		if age >= stuckTerminatingThreshold {
			label := fmt.Sprintf("%s/%s: terminating for %s", pod.Namespace, pod.Name, age.Round(time.Second))
			stuck = append(stuck, label)
			findings = append(findings, label)
			if nsCache.IsPlatform(ctx, pod.Namespace) {
				serviceTeamPod = true
			}
		}
	}

	if len(stuck) > 0 {
		owner := upgradev1alpha1.OwnerCustomer
		if serviceTeamPod {
			owner = upgradev1alpha1.OwnerServiceTeam
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d pod(s) have been Terminating for >%s and may block node drain", len(stuck), stuckTerminatingThreshold),
			Findings: findings,
			Owner:    owner,
			Action:   "Identify and force-delete the listed pods only after confirming it is safe: oc delete pod <name> -n <ns> --grace-period=0 --force",
		}
	}
	return CheckOutput{
		Result:  upgradev1alpha1.CheckPassed,
		Summary: "No pods stuck in Terminating state",
		Findings: []string{
			fmt.Sprintf("Checked %d pod(s): none have been Terminating for more than %s", len(pods.Items), stuckTerminatingThreshold),
		},
	}
}

