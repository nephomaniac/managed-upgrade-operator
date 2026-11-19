import type { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

export type CheckResult = 'Passed' | 'Warning' | 'Failed' | 'Error' | 'Skipped' | 'TimedOut' | 'Unknown';
export type CheckOwner = 'customer' | 'service-team';
export type CheckExecutionState = 'Completed' | 'Error' | 'Skipped' | 'TimedOut';

export interface CheckExecution {
  state: CheckExecutionState;
  startedAt?: string;
  completedAt?: string;
  summary?: string;
  findings?: string[];
  error?: string;
}

export interface DiagsCheckResult {
  name: string;
  description?: string;
  result: CheckResult;
  owner?: CheckOwner;
  ownerLabel?: string;
  action?: string;
  timeoutSeconds: number;
  execution: CheckExecution;
}

export interface UpgradeDiagsCheckStatus {
  startedAt?: string;
  completedAt?: string;
  overallResult?: CheckResult;
  checksPassed?: number;
  checksWarned?: number;
  checksFailed?: number;
  checksErrored?: number;
  checks?: DiagsCheckResult[];
}

export interface UpgradeDiagsCheck extends K8sResourceCommon {
  spec: {
    desired: { version: string; channel: string };
    type: 'OSD' | 'ARO';
    checks?: string[];
  };
  status?: UpgradeDiagsCheckStatus;
}
