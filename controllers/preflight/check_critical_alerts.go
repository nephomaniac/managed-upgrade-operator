package preflight

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	"github.com/openshift/managed-upgrade-operator/pkg/metrics"
	"github.com/openshift/managed-upgrade-operator/pkg/upgraders"
)

const (
	descCriticalAlerts         = "Checks for critical alerts that would block an upgrade, using MUO's default alert rules. Queries Prometheus for firing critical alerts."
	descCriticalAlertsExtended = "Detailed critical alert analysis without ignore filters. Lists all firing critical alerts in core OpenShift namespaces (openshift-*, kube-*, default)."
)

type criticalAlertsCheck struct{}

func (c *criticalAlertsCheck) Name() string        { return "CriticalAlerts" }
func (c *criticalAlertsCheck) Description() string { return descCriticalAlerts }
func (c *criticalAlertsCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

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
	findings := []string{"Uses MUO's upgrade validation logic (upgraders.CriticalAlertsDefault)"}
	fakeUC := createFakeUpgradeConfig("4.15.0")
	passed, err := upgraders.CriticalAlertsDefault(metricsClient, fakeUC, logger, "")
	if err != nil || !passed {
		msg := "critical alerts firing"
		if err != nil {
			msg = err.Error()
		}
		return CheckOutput{
			Result:   upgradev1alpha1.CheckFailed,
			Summary:  fmt.Sprintf("Upgrade validation failed: %s", msg),
			Findings: findings,
			Action:   "Resolve the firing critical alerts before attempting an upgrade.",
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckPassed,
		Summary:  "Upgrade validation passed: no critical alerts firing",
		Findings: findings,
	}
}

type criticalAlertsExtendedCheck struct{}

func (c *criticalAlertsExtendedCheck) Name() string        { return "CriticalAlertsExtended" }
func (c *criticalAlertsExtendedCheck) Description() string { return descCriticalAlertsExtended }
func (c *criticalAlertsExtendedCheck) DefaultOwner() upgradev1alpha1.CheckOwner {
	return upgradev1alpha1.OwnerCustomer
}

func (c *criticalAlertsExtendedCheck) Run(ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) CheckOutput {
	var alerts *metrics.AlertResponse
	var err error
	query := `ALERTS{alertstate="firing",severity="critical",namespace=~"^openshift.*|^kube-.*|^default$"}`

	for attempt := 1; attempt <= prometheusRetries; attempt++ {
		var metricsClient metrics.Metrics
		metricsClient, err = deps.MetricsClientBuilder.NewClient(deps.Client)
		if err != nil {
			if attempt < prometheusRetries {
				select {
				case <-ctx.Done():
					return CheckOutput{
						Result: upgradev1alpha1.CheckResultError,
						Error:  fmt.Sprintf("timed out waiting for metrics client: %v", err),
						Owner:  upgradev1alpha1.OwnerServiceTeam,
						Action: "Contact Red Hat support if Prometheus cannot be reached.",
					}
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		alerts, err = metricsClient.Query(query)
		if err != nil {
			if attempt < prometheusRetries {
				select {
				case <-ctx.Done():
					return CheckOutput{
						Result: upgradev1alpha1.CheckResultError,
						Error:  fmt.Sprintf("timed out querying Prometheus: %v", err),
						Owner:  upgradev1alpha1.OwnerServiceTeam,
						Action: "Contact Red Hat support if Prometheus cannot be reached.",
					}
				case <-time.After(prometheusRetryBase * time.Duration(attempt)):
				}
			}
			continue
		}
		break
	}
	if err != nil {
		return CheckOutput{
			Result: upgradev1alpha1.CheckResultError,
			Error:  fmt.Sprintf("unable to query critical alerts after %d attempts: %v", prometheusRetries, err),
			Owner:  upgradev1alpha1.OwnerServiceTeam,
			Action: "Contact Red Hat support if Prometheus metrics cannot be queried.",
		}
	}
	if len(alerts.Data.Result) == 0 {
		return CheckOutput{
			Result:   upgradev1alpha1.CheckPassed,
			Summary:  "No critical alerts firing in core namespaces",
			Findings: []string{"No firing critical alerts in openshift-*, kube-*, or default namespaces"},
		}
	}
	seen := make(map[string]bool)
	var names []string
	for _, a := range alerts.Data.Result {
		name := a.Metric["alertname"]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return CheckOutput{
		Result:   upgradev1alpha1.CheckFailed,
		Summary:  fmt.Sprintf("%d critical alert(s) firing: %s", len(names), strings.Join(names, ", ")),
		Findings: names,
		Action:   "Investigate and resolve the listed critical alerts before upgrading.",
	}
}
