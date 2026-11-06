package preflight

// registeredChecks is the ordered list of preflight checks that run against the cluster.
//
// To add a new check:
//  1. Implement PreflightCheck in the appropriate check_<group>.go file.
//  2. Append it here — that's all.
//
// The framework (runCheck) handles timeout, panic recovery, timing, Description,
// and Owner injection automatically.
var registeredChecks = []PreflightCheck{
	&clusterVersionCheck{},
	&upgradeInFlightCheck{},
	&clusterOperatorsCheck{},
	&clusterOperatorsExtendedCheck{},
	&criticalAlertsCheck{},
	&criticalAlertsExtendedCheck{},
	&podDisruptionBudgetsCheck{},
	&pdbMasterInfraExtendedCheck{},
	&manuallyCordonedNodesCheck{},
	&manuallyCordonedNodesExtendedCheck{},
	&nodeTaintsCheck{},
	&nodeTaintsExtendedCheck{},
	&upgradeableConditionCheck{},
	&pullSecretCheck{},
	&adminAcksCheck{},
}
