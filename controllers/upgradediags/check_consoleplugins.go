package upgradediags

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const descConsolePluginReferences = "Lists ConsolePlugin CRs and verifies that each referenced Service exists in the specified namespace. Orphaned plugin references cause the console operator to report degraded, which blocks the upgrade health gate. Source: OHSS Case 5 (OHSS-55964: invalid console plugin references)."

var consolePluginListGVK = schema.GroupVersionKind{Group: "console.openshift.io", Version: "v1", Kind: "ConsolePluginList"}

type consolePluginReferencesCheck struct{}

func (c *consolePluginReferencesCheck) Name() string                 { return "ConsolePluginReferences" }
func (c *consolePluginReferencesCheck) Description() string           { return descConsolePluginReferences }
func (c *consolePluginReferencesCheck) DefaultTimeout() time.Duration { return 30 * time.Second }
func (c *consolePluginReferencesCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *consolePluginReferencesCheck) UpgradePhase() string { return "operators" }

func (c *consolePluginReferencesCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	pluginList := &unstructured.UnstructuredList{}
	pluginList.SetGroupVersionKind(consolePluginListGVK)
	if err := deps.Client.List(ctx, pluginList, &client.ListOptions{}); err != nil {
		if apierrors.IsNotFound(err) || isNoKindMatchErr(err) {
			return CheckOutput{
				Result:   upgradev1alpha1.CheckPassed,
				Summary:  "ConsolePlugin CRD not present — no plugins to check",
				Findings: []string{"ConsolePlugin API not available on this cluster"},
			}
		}
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to list ConsolePlugins: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if ConsolePlugins cannot be listed.",
		}
	}

	var orphaned []string
	var findings []string

	for _, plugin := range pluginList.Items {
		pluginName := plugin.GetName()
		// spec.backend.service.{name,namespace}
		svcName, _, _ := unstructured.NestedString(plugin.Object, "spec", "backend", "service", "name")
		svcNS, _, _ := unstructured.NestedString(plugin.Object, "spec", "backend", "service", "namespace")

		if svcName == "" || svcNS == "" {
			findings = append(findings, fmt.Sprintf("%s: no backend service reference (skipped)", pluginName))
			continue
		}

		svc := &corev1.Service{}
		err := deps.Client.Get(ctx, types.NamespacedName{Name: svcName, Namespace: svcNS}, svc)
		if err != nil {
			if apierrors.IsNotFound(err) {
				label := fmt.Sprintf("%s: references Service %s/%s which does not exist", pluginName, svcNS, svcName)
				orphaned = append(orphaned, label)
				findings = append(findings, label)
			} else {
				findings = append(findings, fmt.Sprintf("%s: error checking Service %s/%s: %v", pluginName, svcNS, svcName, err))
			}
		} else {
			findings = append(findings, fmt.Sprintf("%s: Service %s/%s exists", pluginName, svcNS, svcName))
		}
	}

	if len(orphaned) > 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("%d ConsolePlugin(s) reference Services that do not exist", len(orphaned)),
			Findings: findings,
			Action:   "Remove orphaned ConsolePlugin CRs: oc delete consoleplugin <name>. The console operator will degrade until these are removed.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  fmt.Sprintf("All %d ConsolePlugin(s) reference valid Services", len(pluginList.Items)),
		Findings: findings,
	}
}

// isNoKindMatchErr returns true for "no kind is registered" errors (CRD not installed).
func isNoKindMatchErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no kind is registered") || strings.Contains(msg, "no matches for kind")
}
