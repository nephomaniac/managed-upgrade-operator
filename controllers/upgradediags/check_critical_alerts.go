package upgradediags

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
)

const (
	descCriticalAlerts = "Runs the same critical alert gate MUO uses before starting an upgrade. " +
		"Reports whether any blocking critical alerts are currently firing. " +
		"See CriticalAlertsExtended for the complete list of all firing critical alerts with namespace context."

	descCriticalAlertsExtended = "Comprehensive scan for all critical alerts currently firing across core " +
		"OpenShift namespaces (openshift-*, kube-*, default). The findings list every alert found at time of " +
		"check — it is not a fixed allowlist. A short findings list means few alerts are firing, not that " +
		"only those specific alerts were checked. This check does not apply MUO's internal alert suppression rules."
)

type criticalAlertsCheck struct{}

func (c *criticalAlertsCheck) Name() string                 { return "CriticalAlerts" }
func (c *criticalAlertsCheck) Description() string           { return descCriticalAlerts }
func (c *criticalAlertsCheck) DefaultTimeout() time.Duration { return 0 }
func (c *criticalAlertsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *criticalAlertsCheck) UpgradePhase() string { return "pre-checks" }

func (c *criticalAlertsCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	metricsClient, err := deps.MetricsClientBuilder.NewClient(deps.Client)
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("failed to create metrics client: %v", err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if Prometheus metrics cannot be queried.",
		}
	}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.CriticalAlertsDefault(metricsClient, fakeUC, logger, "")
	if err != nil || !passed {
		msg := "critical alerts firing"
		if err != nil {
			msg = err.Error()
		}
		return CheckOutput{
			Result:  upgradev1alpha1.CheckFailed,
			Summary: fmt.Sprintf("Upgrade validation failed: %s", msg),
			Findings: []string{
				"Review CriticalAlertsExtended for the full list of firing alerts with namespace context.",
			},
			Action: "Resolve the firing critical alerts before attempting an upgrade.",
		}
	}
	return CheckOutput{
		Result:  upgradev1alpha1.CheckPassed,
		Summary: "Upgrade validation passed: no critical alerts firing",
	}
}

type criticalAlertsExtendedCheck struct{}

func (c *criticalAlertsExtendedCheck) Name() string                 { return "CriticalAlertsExtended" }
func (c *criticalAlertsExtendedCheck) Description() string           { return descCriticalAlertsExtended }
func (c *criticalAlertsExtendedCheck) DefaultTimeout() time.Duration { return 0 }
func (c *criticalAlertsExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}
func (c *criticalAlertsExtendedCheck) UpgradePhase() string { return "pre-checks" }

func (c *criticalAlertsExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	// QueryAlerts calls /api/v1/alerts (not PromQL) so we get alert annotations
	// (description, summary) alongside metric labels.
	type alertEntry struct {
		labels      map[string]string
		annotations map[string]string
	}
	var matched []alertEntry
	var err error

	for attempt := 1; attempt <= prometheusRetries; attempt++ {
		metricsClient, clientErr := deps.MetricsClientBuilder.NewClient(deps.Client)
		if clientErr != nil {
			err = clientErr
			if attempt < prometheusRetries {
				select {
				case <-ctx.Done():
					return CheckOutput{
						Result: upgradev1alpha1.CheckResultError,
						Error:  fmt.Sprintf("timed out waiting for metrics client: %v", clientErr),
						Owner:  upgradev1alpha1.OwnerServiceTeam,
						Action: "Contact Red Hat support if Prometheus cannot be reached.",
					}
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		infos, queryErr := metricsClient.QueryAlerts()
		if queryErr != nil {
			err = queryErr
			if attempt < prometheusRetries {
				select {
				case <-ctx.Done():
					return CheckOutput{
						Result: upgradev1alpha1.CheckResultError,
						Error:  fmt.Sprintf("timed out querying Prometheus alerts: %v", queryErr),
						Owner:  upgradev1alpha1.OwnerServiceTeam,
						Action: "Contact Red Hat support if Prometheus cannot be reached.",
					}
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		for _, a := range infos {
			if a.State != "firing" || a.Labels["severity"] != "critical" {
				continue
			}
			if !isCoreNamespace(a.Labels["namespace"]) {
				continue
			}
			matched = append(matched, alertEntry{labels: a.Labels, annotations: a.Annotations})
		}
		err = nil
		break
	}
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to query Prometheus alerts after %d attempts: %v", prometheusRetries, err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if Prometheus metrics cannot be queried.",
		}
	}
	if len(matched) == 0 {
		return CheckOutput{
			Result:  upgradev1alpha1.CheckPassed,
			Summary: "No critical alerts firing in scanned namespaces (openshift-*, kube-*, default)",
		}
	}

	type alertKey struct{ name, ns string }
	seen := make(map[alertKey]bool)
	uniqueNames := make(map[string]bool)
	var findings []string

	for _, a := range matched {
		name := a.labels["alertname"]
		ns := a.labels["namespace"]
		key := alertKey{name, ns}
		if seen[key] {
			continue
		}
		seen[key] = true
		uniqueNames[name] = true

		// Build a human-readable finding: alertname — description (from annotations)
		// Fall back to summary if description is absent, then just namespace.
		finding := name
		if ns != "" {
			finding += fmt.Sprintf(" (namespace: %s)", ns)
		}
		if desc := a.annotations["description"]; desc != "" {
			finding += " — " + desc
		} else if summary := a.annotations["summary"]; summary != "" {
			finding += " — " + summary
		}
		findings = append(findings, finding)
	}
	sort.Strings(findings)

	uniqueNameList := make([]string, 0, len(uniqueNames))
	for n := range uniqueNames {
		uniqueNameList = append(uniqueNameList, n)
	}
	sort.Strings(uniqueNameList)

	return CheckOutput{
		// Warning, not Failed: this extended scan does not apply MUO's internal
		// alert-suppression rules, so it may surface alerts that MUO would allow
		// through. Use CriticalAlerts (which wraps upgraders.CriticalAlertsDefault)
		// for the authoritative upgrade gate.
		Result:   upgradev1alpha1.CheckResultWarning,
		Summary:  fmt.Sprintf("%d critical alert(s) firing (advisory — does not apply MUO suppression rules): %s", len(uniqueNameList), strings.Join(uniqueNameList, ", ")),
		Findings: findings,
		Action:   "Review the listed critical alerts. Check the CriticalAlerts result for the authoritative MUO upgrade gate. See OpenShift Console → Observe → Alerting for runbook links.",
	}
}

// isCoreNamespace returns true for namespaces scanned by the critical alerts check
// (mirrors the PromQL filter: namespace=~"^openshift.*|^kube-.*|^default$").
func isCoreNamespace(ns string) bool {
	return strings.HasPrefix(ns, "openshift") ||
		strings.HasPrefix(ns, "kube-") ||
		ns == "default"
}
