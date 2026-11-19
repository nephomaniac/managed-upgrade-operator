package upgradediags

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	prometheusRetries   = 3
	defaultCheckTimeout = 30 * time.Second
)

var prometheusRetryBase = 2 * time.Second

// resolveCheckTimeout returns the timeout to apply to a single check.
// Priority: check.DefaultTimeout() > spec.DefaultCheckTimeoutSeconds > 30s constant.
func resolveCheckTimeout(check DiagnosticsCheck, spec upgradev1alpha1.UpgradeDiagsCheckSpec) time.Duration {
	if d := check.DefaultTimeout(); d > 0 {
		return d
	}
	if spec.DefaultCheckTimeoutSeconds > 0 {
		return time.Duration(spec.DefaultCheckTimeoutSeconds) * time.Second
	}
	return defaultCheckTimeout
}

// runCheck executes a ReadinessCheck with timeout, panic recovery, timing, and
// framework-level field injection (Description, Owner, Action). Check authors
// only need to return a CheckOutput.
func runCheck(check DiagnosticsCheck, ctx context.Context, deps CheckDependencies, logger logr.Logger, timeout time.Duration) (result upgradev1alpha1.DiagsCheckResult) {
	start := metav1.Now()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		if p := recover(); p != nil {
			result = withTiming(
				withAction(withOwner(withDesc(
					errorResult(check.Name(), fmt.Sprintf("check panicked: %v", p), timeout),
					check.Description()),
					upgradev1alpha1.OwnerServiceTeam),
					"Contact Red Hat support — this check panicked unexpectedly."),
				start, metav1.Now())
		}
	}()

	output := check.Run(checkCtx, deps, logger, timeout)

	owner := output.Owner
	if owner == "" {
		owner = check.DefaultOwner()
	}

	var r upgradev1alpha1.DiagsCheckResult
	if output.Result == upgradev1alpha1.CheckResultError {
		errMsg := output.Error
		if errMsg == "" {
			errMsg = "check returned an error result without an error message"
		}
		r = errorResult(check.Name(), errMsg, timeout)
	} else {
		r = completedResult(check.Name(), output.Result, output.Summary, output.Findings, timeout)
	}
	r = withDesc(r, check.Description())
	r = withOwner(r, owner)
	if output.Action != "" {
		r = withAction(r, output.Action)
	}
	if len(output.StructuredFindings) > 0 {
		r.StructuredFindings = output.StructuredFindings
		// Auto-populate string Findings from structured findings for backwards compat.
		if len(r.Execution.Findings) == 0 {
			for _, sf := range output.StructuredFindings {
				r.Execution.Findings = append(r.Execution.Findings, sf.Message)
			}
		}
	}
	return withTiming(r, start, metav1.Now())
}

func timedOutResult(check DiagnosticsCheck, timeout time.Duration) upgradev1alpha1.DiagsCheckResult {
	return withDesc(upgradev1alpha1.DiagsCheckResult{
		Name:           check.Name(),
		Result:         upgradev1alpha1.CheckResultTimedOut,
		Owner:          upgradev1alpha1.OwnerServiceTeam,
		Action:         "Increase spec.overallTimeoutSeconds or reduce the number of checks.",
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution:      upgradev1alpha1.CheckExecution{State: upgradev1alpha1.CheckTimedOut},
	}, check.Description())
}

func skippedResult(check DiagnosticsCheck, timeout time.Duration) upgradev1alpha1.DiagsCheckResult {
	return withDesc(upgradev1alpha1.DiagsCheckResult{
		Name:           check.Name(),
		Result:         upgradev1alpha1.CheckResultSkipped,
		Owner:          check.DefaultOwner(),
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution:      upgradev1alpha1.CheckExecution{State: upgradev1alpha1.CheckSkipped},
	}, check.Description())
}

func completedResult(name string, result upgradev1alpha1.CheckResult, summary string, findings []string, timeout time.Duration) upgradev1alpha1.DiagsCheckResult {
	return upgradev1alpha1.DiagsCheckResult{
		Name:           name,
		Result:         result,
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution: upgradev1alpha1.CheckExecution{
			State:    upgradev1alpha1.CheckCompleted,
			Summary:  summary,
			Findings: findings,
		},
	}
}

func errorResult(name, errMsg string, timeout time.Duration) upgradev1alpha1.DiagsCheckResult {
	return upgradev1alpha1.DiagsCheckResult{
		Name:           name,
		Result:         upgradev1alpha1.CheckResultError,
		TimeoutSeconds: int32(timeout.Seconds()),
		Execution: upgradev1alpha1.CheckExecution{
			State: upgradev1alpha1.CheckError,
			Error: errMsg,
		},
	}
}

func withDesc(r upgradev1alpha1.DiagsCheckResult, desc string) upgradev1alpha1.DiagsCheckResult {
	r.Description = desc
	return r
}

func withTiming(r upgradev1alpha1.DiagsCheckResult, start, end metav1.Time) upgradev1alpha1.DiagsCheckResult {
	r.Execution.StartedAt = &start
	r.Execution.CompletedAt = &end
	return r
}

func withOwner(r upgradev1alpha1.DiagsCheckResult, owner upgradev1alpha1.CheckOwner) upgradev1alpha1.DiagsCheckResult {
	r.Owner = owner
	return r
}

func withAction(r upgradev1alpha1.DiagsCheckResult, action string) upgradev1alpha1.DiagsCheckResult {
	r.Action = action
	return r
}

func createFakeUpgradeConfig(targetVersion string) *upgradev1alpha1.UpgradeConfig {
	return &upgradev1alpha1.UpgradeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "preflight-check"},
		Spec:       upgradev1alpha1.UpgradeConfigSpec{Desired: upgradev1alpha1.Update{Version: targetVersion}},
		Status: upgradev1alpha1.UpgradeConfigStatus{
			History: upgradev1alpha1.UpgradeHistories{
				{Version: targetVersion, Phase: upgradev1alpha1.UpgradePhaseNew},
			},
		},
	}
}
