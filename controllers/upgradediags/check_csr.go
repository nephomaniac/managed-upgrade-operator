package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	certificatesv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const descPendingCSRs = "Lists CertificateSigningRequests in Pending state. Pending CSRs block replacement worker nodes from joining the cluster after machine replacement during the worker upgrade phase. Source: OHSS Case 3 (OHSS-57583: CSR approval failure after CA rotation)."

type pendingCSRsCheck struct{}

func (c *pendingCSRsCheck) Name() string                 { return "PendingCSRs" }
func (c *pendingCSRsCheck) Description() string           { return descPendingCSRs }
func (c *pendingCSRsCheck) DefaultTimeout() time.Duration { return 0 }
func (c *pendingCSRsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerServiceTeam
}
func (c *pendingCSRsCheck) UpgradePhase() string { return "worker-upgrade" }

func (c *pendingCSRsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	csrList := &certificatesv1.CertificateSigningRequestList{}
	if err := deps.Client.List(ctx, csrList, &client.ListOptions{}); err != nil {
		if apierrors.IsForbidden(err) {
			return CheckOutput{
				Result:   upgradev1alpha1.CheckResultWarning,
				Summary:  "RBAC not granted — cannot list CertificateSigningRequests",
				Findings: []string{fmt.Sprintf("forbidden: %v", err)},
				Owner:    upgradev1alpha1.OwnerServiceTeam,
				Action:   "Add 'certificatesigningrequests' get+list permission for 'certificates.k8s.io' to the managed-upgrade-operator ClusterRole.",
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list CertificateSigningRequests: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if CSRs cannot be listed.",
		}
	}

	var pending []string
	var findings []string
	for i := range csrList.Items {
		csr := &csrList.Items[i]
		approved := false
		denied := false
		for _, cond := range csr.Status.Conditions {
			if cond.Type == certificatesv1.CertificateApproved {
				approved = true
			}
			if cond.Type == certificatesv1.CertificateDenied {
				denied = true
			}
		}
		if !approved && !denied {
			label := fmt.Sprintf("%s: pending (username=%s, signerName=%s)", csr.Name, csr.Spec.Username, csr.Spec.SignerName)
			pending = append(pending, csr.Name)
			findings = append(findings, label)
		}
	}

	if len(pending) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d CertificateSigningRequest(s) pending approval — may block new nodes from joining", len(pending)),
			Findings: findings,
			Action:   "Contact Red Hat support. Do not approve CSRs manually without verification of the requesting node identity.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("No pending CSRs (checked %d total)", len(csrList.Items)),
		Findings: []string{fmt.Sprintf("All %d CertificateSigningRequest(s) are approved, denied, or already have a certificate", len(csrList.Items))},
	}
}
