package upgradediags

// registeredChecks is the ordered list of readiness checks that run against the cluster.
//
// To add a new check:
//  1. Implement ReadinessCheck in the appropriate check_<group>.go file.
//  2. Append it here — that's all.
//
// The framework (runCheck) handles timeout, panic recovery, timing, Description,
// and Owner injection automatically.
var registeredChecks = []DiagnosticsCheck{
	// ---- Upgrade eligibility ----
	&clusterVersionCheck{},
	&upgradeTargetCheck{},
	&upgradeTargetExtendedCheck{},
	&upgradeInFlightCheck{},
	&upgradeableConditionCheck{},
	&adminAcksCheck{},
	&pullSecretCheck{},
	// ---- Cluster health gates ----
	&clusterOperatorsCheck{},
	&clusterOperatorsExtendedCheck{},
	&criticalAlertsCheck{},
	&criticalAlertsExtendedCheck{},
	// ---- Node readiness ----
	&workerNodeReadinessCheck{},
	&manuallyCordonedNodesCheck{},
	&manuallyCordonedNodesExtendedCheck{},
	&nodeTaintsCheck{},
	&nodeTaintsExtendedCheck{},
	&pendingCSRsCheck{},
	// ---- Workload / drain blockers ----
	&podDisruptionBudgetsCheck{},
	&pdbMasterInfraExtendedCheck{},
	&pdbDrainSafetyCheck{},
	&stuckDrainingNodesCheck{},
	&stuckTerminatingPodsCheck{},
	// ---- Configuration compatibility ----
	&pausedMachineConfigPoolCheck{},
	&installedOperatorMaxVersionCheck{},
	&consolePluginReferencesCheck{},
	&clusterAutoscalerConfigCheck{},
	&upgradeConfigConsistencyCheck{},
	// ---- OCM API checks ----
	&ocmLimitedSupportCheck{},
	&ocmUpgradePolicyCheck{},
	&ocmVersionGateCheck{},
}
