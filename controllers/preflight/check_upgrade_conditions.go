package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/util"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

const (
	descUpgradeableCondition = "Checks if the ClusterVersion has Upgradeable=False. When CVO sets this condition, it means an operator or in-cluster state is actively blocking y-stream upgrades."
	descPullSecretValid      = "Validates the cluster pull secret exists and has the required authentication entries for accessing Red Hat container registries."
	descAdminAcks            = "Validates admin acknowledgments for upgrade gate conditions. For y-stream upgrades, checks the openshift-config/admin-acks ConfigMap for required acknowledgements."
)

type upgradeableConditionCheck struct{}

func (c *upgradeableConditionCheck) Name() string                 { return "UpgradeableCondition" }
func (c *upgradeableConditionCheck) Description() string           { return descUpgradeableCondition }
func (c *upgradeableConditionCheck) DefaultTimeout() time.Duration { return 0 }
func (c *upgradeableConditionCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *upgradeableConditionCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	cvClient := deps.CvClientBuilder.New(deps.Client)
	clusterVersion, err := cvClient.GetClusterVersion()
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to get cluster version: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if ClusterVersion cannot be queried.",
		}
	}
	for _, condition := range clusterVersion.Status.Conditions {
		if condition.Type == configv1.OperatorUpgradeable && condition.Status == configv1.ConditionFalse {
			return CheckOutput{
				Result:  upgradev1alpha1.CheckFailed,
				Summary: fmt.Sprintf("Cluster not upgradeable: %s", condition.Reason),
				Findings: []string{
					fmt.Sprintf("Upgradeable=False: reason=%s", condition.Reason),
					fmt.Sprintf("message=%s", condition.Message),
				},
				Action: "Resolve the Upgradeable=False condition. For AdminAck gates, use the ROSA CLI or update the admin-acks ConfigMap.",
			}
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Cluster is upgradeable",
		Findings: []string{"Upgradeable condition is True (or not set)"},
	}
}

type pullSecretCheck struct{}

func (c *pullSecretCheck) Name() string                 { return "PullSecretValid" }
func (c *pullSecretCheck) Description() string           { return descPullSecretValid }
func (c *pullSecretCheck) DefaultTimeout() time.Duration { return 0 }
func (c *pullSecretCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *pullSecretCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	_, err := util.GetAccessToken(deps.Client)
	if err != nil {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  "Pull secret validation failed",
			Findings: []string{err.Error()},
			Action:   "Renew the cluster pull secret at cloud.openshift.com and update openshift-config/pull-secret.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Pull secret is valid",
		Findings: []string{"cloud.openshift.com auth entry is present in the cluster pull secret"},
	}
}

type adminAcksCheck struct{}

func (c *adminAcksCheck) Name() string                 { return "AdminAcks" }
func (c *adminAcksCheck) Description() string           { return descAdminAcks }
func (c *adminAcksCheck) DefaultTimeout() time.Duration { return 0 }
func (c *adminAcksCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *adminAcksCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	isYStream := deps.DesiredVersion != ""

	cm := &corev1.ConfigMap{}
	err := deps.Client.Get(ctx, types.NamespacedName{Namespace: "openshift-config", Name: "admin-acks"}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if isYStream {
				return CheckOutput{
					Result:   upgradev1alpha1.CheckFailed,
					Summary:  "admin-acks ConfigMap not found — required for y-stream upgrades",
					Findings: []string{"ConfigMap openshift-config/admin-acks does not exist"},
					Action:   "Acknowledge the required upgrade gates. For ROSA, use `rosa upgrade cluster -c <cluster-id>`.",
				}
			}
			return CheckOutput{
				Result:   upgradev1alpha1.CheckPassed,
				Summary:  "Admin acks not required for z-stream upgrade",
				Findings: []string{"ConfigMap openshift-config/admin-acks does not exist (not required for z-stream)"},
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to read admin-acks ConfigMap: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if the admin-acks ConfigMap cannot be read.",
		}
	}

	var ackKeys []string
	for k := range cm.Data {
		ackKeys = append(ackKeys, fmt.Sprintf("%s=%s", k, cm.Data[k]))
	}
	if isYStream && len(cm.Data) == 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  "admin-acks ConfigMap exists but has no entries",
			Findings: []string{"ConfigMap openshift-config/admin-acks has no data entries"},
			Action:   "Acknowledge the required upgrade gates. For ROSA, use `rosa upgrade cluster -c <cluster-id>`.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("Admin acks present: %d entry/entries in openshift-config/admin-acks", len(ackKeys)),
		Findings: ackKeys,
	}
}
