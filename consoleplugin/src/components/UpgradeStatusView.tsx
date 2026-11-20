import * as React from 'react';
import { Alert, Button, Spinner } from '@patternfly/react-core';
import { SyncAltIcon, CaretDownIcon, CaretRightIcon } from '@patternfly/react-icons';
import { consoleFetch, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { type UpgradeDiagsCheck, type DiagsCheckResult, type UpgradeHistoryResponse } from '../types';

const STATUS_API  = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/upgrade-status';
const HISTORY_API = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/upgrade-status/history';
const VERSION_API = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/version';
const MUO_NS = 'openshift-managed-upgrade-operator';

// PF semantic design tokens — single source of truth for all colors in this file.
const T = {
  danger:     'var(--pf-t--global--color--status--danger--default)',
  warning:    'var(--pf-t--global--color--status--warning--default)',
  success:    'var(--pf-t--global--color--status--success--default)',
  info:       'var(--pf-t--global--color--status--info--default)',
  textSubtle: 'var(--pf-t--global--color--text--subtle)',
  textMuted:  'var(--pf-t--global--color--text--placeholder)',
  border:     'var(--pf-t--global--border--color--default)',
  bgSecondary:'var(--pf-t--global--background--color--secondary--default)',
  shadow:     'var(--pf-t--global--shadow--md)',
  dangerBg:   'var(--pf-t--global--background--color--status--danger--default)',
  warningBg:  'var(--pf-t--global--background--color--status--warning--default)',
  infoBg:     'var(--pf-t--global--background--color--status--info--default)',
  successBg:  'var(--pf-t--global--background--color--status--success--default)',
  headerBg:   'var(--pf-t--global--background--color--secondary--default)',
  radiusPill: 'var(--pf-t--global--border--radius--pill)',
  radiusLg:   'var(--pf-t--global--border--radius--lg)',
  radiusMd:   'var(--pf-t--global--border--radius--md)',
  radiusSm:   'var(--pf-t--global--border--radius--sm)',
} as const;

// ---- Types (mirror upgrade_status.go response) ----------------------------

interface DrainBlocker {
  podName: string;
  podNamespace: string;
  pdbName: string;
  owner?: string;
  disruptionsAllowed: number;
  desiredHealthy: number;
  currentHealthy: number;
  violation?: string;
}

interface ProviderError {
  source: string;
  reason?: string;
  message: string;
  since?: string;
}

interface NodeCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  since?: string;
}

interface NodeStatus {
  name: string;
  phase: string;
  currentConfig?: string;
  desiredConfig?: string;
  roles?: string[];
  phaseSince?: string;
  elapsedSeconds?: number;
  ageSeconds?: number;
  drainBlockers?: DrainBlocker[];
  providerErrors?: ProviderError[];
  conditions?: NodeCondition[];
}

interface MachinePoolStatus {
  name: string;
  phase: string;
  total: number;
  updated: number;
  ready: number;
  unavailable: number;
  degraded: number;
  startedAt?: string;
  completedAt?: string;
  elapsedSeconds?: number;
  nodes?: NodeStatus[];
}

interface OperatorCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  since?: string;
}

interface OperatorStatus {
  name: string;
  phase: string;
  version?: string;
  available: boolean;
  progressing: boolean;
  degraded: boolean;
  message?: string;
  progressingSince?: string;
  elapsedInPhaseSeconds?: number;
  conditions?: OperatorCondition[];
}

interface OperatorsStatus {
  phase: string;
  total: number;
  available: number;
  progressing: number;
  degraded: number;
  startedAt?: string;
  elapsedSeconds?: number;
  operators: OperatorStatus[];
}

interface ControlPlaneStatus {
  phase: string;
  startedAt?: string;
  completedAt?: string;
  elapsedSeconds?: number;
  message?: string;
}

interface UpgradeBlocker {
  type: string;
  component: string;
  message: string;
  since?: string;
  elapsedSeconds?: number;
}

interface MachineHealthCheckStatus {
  name: string;
  maxUnhealthy: string;
  expectedMachines: number;
  currentHealthy: number;
  remediationsAllowed: number;
  remediating: boolean;
}

interface UpgradeSnapshot {
  phase: string;
  targetVersion?: string;
  targetChannel?: string;
  currentVersion?: string;
  cvoMessage?: string;
  startedAt?: string;
  elapsedSeconds?: number;
  controlPlane: ControlPlaneStatus;
  clusterOperators: OperatorsStatus;
  machinePools: MachinePoolStatus[];
  blockers?: UpgradeBlocker[];
  machineHealthChecks?: MachineHealthCheckStatus[];
}

// ---- Colour helpers --------------------------------------------------------

const PHASE_COLORS: Record<string, string> = {
  'complete':       T.success,
  'updated':        T.success,
  'done':           T.success,
  'available':      T.success,
  'not-started':    T.textSubtle,
  'upgrading':      T.info,
  'updating':       T.info,
  'control-plane':  T.info,
  'operators':      T.info,
  'worker-upgrade': T.info,
  'progressing':    T.info,
  'working':        T.info,
  'rebooting':      T.warning,
  'draining':       T.warning,
  'pre-checks':     T.warning,
  'stalled':        T.warning,
  'degraded':       T.danger,
  'failed':         T.danger,
};

const phaseColor  = (p: string) => PHASE_COLORS[p] ?? T.textSubtle;
const phaseLabel  = (p: string) => p.replace(/-/g, ' ').replace(/\b\w/g, c => c.toUpperCase());

const PhaseBadge: React.FC<{ phase: string; size?: 'sm' | 'md' }> = ({ phase, size = 'sm' }) => {
  const color = phaseColor(phase);
  return (
    <span style={{
      display: 'inline-block',
      padding: size === 'md' ? '3px 12px' : '1px 8px',
      borderRadius: T.radiusPill,
      background: `${color}22`,
      color,
      border: `1px solid ${color}55`,
      fontWeight: 600,
      fontSize: size === 'md' ? 13 : 11,
      whiteSpace: 'nowrap',
    }}>
      {phaseLabel(phase)}
    </span>
  );
};

// ---- Elapsed time formatting -----------------------------------------------

const fmtElapsed = (secs?: number): string => {
  if (!secs || secs <= 0) return '—';
  if (secs < 60) return `${secs}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${secs % 60}s`;
  return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
};

// ---- Section header --------------------------------------------------------

const SectionHeader: React.FC<{ title: string; badge?: React.ReactNode; meta?: string }> = ({ title, badge, meta }) => (
  <div style={{
    display: 'flex', alignItems: 'center', gap: 10,
    padding: '8px 16px',
    background: T.bgSecondary,
    borderBottom: `1px solid ${T.border}`,
    fontSize: 12, fontWeight: 700, textTransform: 'uppercase', letterSpacing: '0.07em',
    color: T.textSubtle,
  }}>
    <span>{title}</span>
    {badge}
    {meta && <span style={{ marginLeft: 'auto', fontWeight: 400, textTransform: 'none', letterSpacing: 0 }}>{meta}</span>}
  </div>
);

// ---- Operators table -------------------------------------------------------

const fmtTime = (ts?: string) => ts ? new Date(ts).toLocaleString() : '—';

const OperatorsTable: React.FC<{ ops: OperatorsStatus }> = ({ ops }) => {
  const [expanded, setExpanded] = React.useState<Record<string, boolean>>({});
  const toggle = (name: string) => setExpanded(prev => ({ ...prev, [name]: !prev[name] }));

  const sorted = [...ops.operators].sort((a, b) => {
    const rank = (o: OperatorStatus) => o.degraded ? 0 : o.progressing ? 1 : 2;
    return rank(a) - rank(b);
  });

  const th: React.CSSProperties = {
    padding: '8px 12px', textAlign: 'left', fontSize: 11, fontWeight: 600,
    textTransform: 'uppercase', letterSpacing: '0.05em',
    color: T.textSubtle,
    borderBottom: `1px solid ${T.border}`,
    background: T.bgSecondary,
    whiteSpace: 'nowrap',
  };

  const condTh: React.CSSProperties = {
    ...th, background: T.dangerBg, fontSize: 10, padding: '5px 10px',
  };

  return (
    <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
      <thead>
        <tr>
          <th style={{ ...th, width: 28, padding: '8px 6px' }} />
          <th style={th}>Operator</th>
          <th style={th}>Version</th>
          <th style={th}>Phase</th>
          <th style={th}>Elapsed</th>
          <th style={{ ...th, width: '40%' }}>Message</th>
        </tr>
      </thead>
      <tbody>
        {sorted.map((op) => {
          const color = phaseColor(op.phase);
          const isExpanded = !!expanded[op.name];
          const hasConditions = (op.conditions?.length ?? 0) > 0;
          const tdStyle: React.CSSProperties = {
            padding: '8px 12px',
            borderBottom: isExpanded ? 'none' : `1px solid ${T.border}`,
            verticalAlign: 'top',
          };
          return (
            <React.Fragment key={op.name}>
              <tr
                onClick={() => hasConditions && toggle(op.name)}
                style={{
                  borderLeft: op.degraded || op.progressing ? `3px solid ${color}` : '3px solid transparent',
                  cursor: hasConditions ? 'pointer' : 'default',
                }}
              >
                <td style={{ ...tdStyle, padding: '8px 6px', textAlign: 'center', color: T.textMuted, fontSize: 11 }}>
                  {hasConditions ? (isExpanded ? '▾' : '▸') : ''}
                </td>
                <td style={{ ...tdStyle, fontWeight: op.degraded || op.progressing ? 600 : 400 }}>{op.name}</td>
                <td style={{ ...tdStyle, color: T.textSubtle, fontFamily: 'monospace', fontSize: 12 }}>
                  {op.version ?? '—'}
                </td>
                <td style={tdStyle}><PhaseBadge phase={op.phase} /></td>
                <td style={{ ...tdStyle, color: T.textSubtle, whiteSpace: 'nowrap' }}>
                  {fmtElapsed(op.elapsedInPhaseSeconds)}
                </td>
                <td style={{ ...tdStyle, color: T.textSubtle, fontSize: 12, wordBreak: 'break-word' }}>
                  {op.message ?? '—'}
                </td>
              </tr>

              {isExpanded && hasConditions && (
                <tr style={{ borderLeft: `3px solid ${color}`, borderBottom: `1px solid ${T.border}` }}>
                  <td colSpan={6} style={{ padding: '0 0 0 28px', background: T.bgSecondary }}>
                    <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
                      <thead>
                        <tr>
                          <th style={condTh}>Condition</th>
                          <th style={condTh}>Status</th>
                          <th style={condTh}>Reason</th>
                          <th style={condTh}>Since</th>
                          <th style={{ ...condTh, width: '45%' }}>Message</th>
                        </tr>
                      </thead>
                      <tbody>
                        {op.conditions!.map((c, i) => {
                          const isUnhealthy = c.status === 'False' && ['Available'].includes(c.type)
                            || c.status === 'True' && ['Progressing', 'Degraded'].includes(c.type);
                          return (
                            <tr key={i} style={{ background: i % 2 === 0 ? 'transparent' : `${T.border}18` }}>
                              <td style={{ padding: '5px 10px', borderBottom: `1px solid ${T.border}`, fontWeight: isUnhealthy ? 700 : 400, color: isUnhealthy ? color : undefined }}>
                                {c.type}
                              </td>
                              <td style={{ padding: '5px 10px', borderBottom: `1px solid ${T.border}`, color: isUnhealthy ? color : T.textSubtle }}>
                                {c.status}
                              </td>
                              <td style={{ padding: '5px 10px', borderBottom: `1px solid ${T.border}`, color: T.textSubtle, fontFamily: 'monospace', fontSize: 11 }}>
                                {c.reason ?? '—'}
                              </td>
                              <td style={{ padding: '5px 10px', borderBottom: `1px solid ${T.border}`, color: T.textMuted, whiteSpace: 'nowrap' }}>
                                {fmtTime(c.since)}
                              </td>
                              <td style={{ padding: '5px 10px', borderBottom: `1px solid ${T.border}`, color: T.textSubtle, wordBreak: 'break-word' }}>
                                {c.message || '—'}
                              </td>
                            </tr>
                          );
                        })}
                      </tbody>
                    </table>
                  </td>
                </tr>
              )}
            </React.Fragment>
          );
        })}
      </tbody>
    </table>
  );
};

// ---- Node table with drill-down -------------------------------------------

const pdbRemediation = (db: DrainBlocker): string => {
  if (db.disruptionsAllowed === 0) {
    return `PDB "${db.pdbName}" allows 0 disruptions. Scale up ${db.owner ? `"${db.owner}"` : 'the workload'} to at least ${db.desiredHealthy + 1} replicas, or temporarily increase MaxUnavailable / decrease MinAvailable.`;
  }
  return `PDB "${db.pdbName}" requires ${db.desiredHealthy} healthy pods but only ${db.currentHealthy} are ready. Wait for pods to recover or adjust the PDB.`;
};

const providerRemediation = (pe: ProviderError): string => {
  const reason = (pe.reason ?? '').toLowerCase();
  const msg    = pe.message.toLowerCase();
  if (reason.includes('iam') || msg.includes('iam') || msg.includes('unauthorized') || msg.includes('access denied')) {
    return 'Check IAM roles and instance profiles. The node may lack permissions to call EC2/EKS APIs.';
  }
  if (reason.includes('capacity') || msg.includes('capacity') || msg.includes('insufficient')) {
    return 'Instance capacity unavailable in this zone. Try a different availability zone or instance type.';
  }
  if (reason.includes('quota') || msg.includes('quota') || msg.includes('limit exceeded')) {
    return 'AWS quota exceeded. Request a limit increase via Service Quotas or free up resources.';
  }
  if (msg.includes('network') || msg.includes('vpc') || msg.includes('subnet') || msg.includes('security group')) {
    return 'Network configuration issue. Check VPC, subnets, security groups, and route tables.';
  }
  if (msg.includes('pull') || msg.includes('image') || msg.includes('registry')) {
    return 'Image pull failure. Verify pull secret validity and registry reachability from this node.';
  }
  if (msg.includes('disk') || msg.includes('storage') || msg.includes('volume') || msg.includes('ebs')) {
    return 'Storage issue. Check EBS volume limits, disk pressure, and persistent volume claims.';
  }
  return 'Review cloud provider logs or the Machine object for additional details.';
};

const NodeDetailPanel: React.FC<{ node: NodeStatus; borderColor: string }> = ({ node, borderColor }) => {
  const hasBlockers       = (node.drainBlockers?.length ?? 0) > 0;
  const hasProviderErrors = (node.providerErrors?.length ?? 0) > 0;
  const hasConditions     = (node.conditions?.length ?? 0) > 0;

  const sectionTitle: React.CSSProperties = {
    fontWeight: 700, fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.07em',
    color: T.textSubtle, marginBottom: 6, marginTop: 10,
  };

  return (
    <div style={{ padding: '10px 12px 12px 20px', background: T.bgSecondary, fontSize: 12 }}>

      {/* PDB drain blockers */}
      {hasBlockers && (
        <>
          <div style={sectionTitle}>PDB Drain Blockers</div>
          {node.drainBlockers!.map((db, i) => (
            <div key={i} style={{
              marginBottom: 8, padding: '8px 12px',
              background: T.dangerBg,
              borderLeft: `3px solid ${T.danger}`,
              borderRadius: T.radiusSm,
            }}>
              <div style={{ fontWeight: 600, color: T.danger, marginBottom: 4 }}>
                {db.podNamespace}/{db.pdbName}
                {db.owner && <span style={{ fontWeight: 400, color: T.textSubtle, marginLeft: 8 }}>via {db.owner}</span>}
              </div>
              <div style={{ color: T.textSubtle, marginBottom: 4 }}>
                disruptions allowed: <strong>{db.disruptionsAllowed}</strong>
                {' · '}healthy: <strong>{db.currentHealthy}/{db.desiredHealthy}</strong>
                {db.violation && <span style={{ marginLeft: 8, fontFamily: 'monospace', color: T.danger }}>[{db.violation}]</span>}
              </div>
              <div style={{ color: T.warning, fontStyle: 'italic' }}>
                ⚑ {pdbRemediation(db)}
              </div>
            </div>
          ))}
        </>
      )}

      {/* Provider / cloud errors */}
      {hasProviderErrors && (
        <>
          <div style={sectionTitle}>Cloud Provider Errors</div>
          {node.providerErrors!.map((pe, i) => (
            <div key={i} style={{
              marginBottom: 8, padding: '8px 12px',
              background: T.warningBg,
              borderLeft: `3px solid ${T.warning}`,
              borderRadius: T.radiusSm,
            }}>
              <div style={{ fontWeight: 600, color: T.warning, marginBottom: 4 }}>
                {pe.source}{pe.reason ? ` / ${pe.reason}` : ''}
              </div>
              <div style={{ color: T.textSubtle, marginBottom: 4 }}>{pe.message}</div>
              <div style={{ color: T.warning, fontStyle: 'italic' }}>
                ⚑ {providerRemediation(pe)}
              </div>
            </div>
          ))}
        </>
      )}

      {/* K8s node conditions */}
      {hasConditions && (
        <>
          <div style={sectionTitle}>Node Conditions</div>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 11 }}>
            <thead>
              <tr>
                {['Condition', 'Status', 'Reason', 'Since', 'Message'].map(h => (
                  <th key={h} style={{
                    padding: '4px 8px', textAlign: 'left', fontWeight: 600,
                    textTransform: 'uppercase', letterSpacing: '0.05em',
                    color: T.textSubtle, borderBottom: `1px solid ${T.border}`,
                  }}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {node.conditions!.map((c, i) => {
                const isUnhealthy = (c.type === 'Ready' && c.status !== 'True')
                  || (c.type !== 'Ready' && c.status === 'True' && ['MemoryPressure','DiskPressure','PIDPressure','NetworkUnavailable'].includes(c.type));
                return (
                  <tr key={i} style={{ background: i % 2 === 0 ? 'transparent' : `${T.border}18` }}>
                    <td style={{ padding: '4px 8px', borderBottom: `1px solid ${T.border}`, fontWeight: isUnhealthy ? 700 : 400, color: isUnhealthy ? borderColor : undefined }}>
                      {c.type}
                    </td>
                    <td style={{ padding: '4px 8px', borderBottom: `1px solid ${T.border}`, color: isUnhealthy ? borderColor : T.textSubtle }}>
                      {c.status}
                    </td>
                    <td style={{ padding: '4px 8px', borderBottom: `1px solid ${T.border}`, color: T.textSubtle, fontFamily: 'monospace' }}>
                      {c.reason ?? '—'}
                    </td>
                    <td style={{ padding: '4px 8px', borderBottom: `1px solid ${T.border}`, color: T.textMuted, whiteSpace: 'nowrap' }}>
                      {fmtTime(c.since)}
                    </td>
                    <td style={{ padding: '4px 8px', borderBottom: `1px solid ${T.border}`, color: T.textSubtle, wordBreak: 'break-word' }}>
                      {c.message || '—'}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </>
      )}

      {!hasBlockers && !hasProviderErrors && !hasConditions && (
        <div style={{ color: T.textMuted }}>No additional details available.</div>
      )}
    </div>
  );
};

const roleColor = (role: string): string => {
  if (role === 'master' || role === 'control-plane') return T.info;
  if (role === 'infra') return T.warning;
  return T.textSubtle; // worker and others
};

// Format an age in seconds into a human-readable string with appropriate
// granularity: seconds, minutes, hours, or days+hours.
const fmtAge = (secs?: number): string => {
  if (secs === undefined || secs <= 0) return '—';
  if (secs < 120)   return `${secs}s`;
  if (secs < 3600)  return `${Math.floor(secs / 60)}m`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  return h > 0 ? `${d}d ${h}h` : `${d}d`;
};

const NodeTable: React.FC<{ nodes: NodeStatus[] }> = ({ nodes }) => {
  const [expandedNodes, setExpandedNodes] = React.useState<Record<string, boolean>>({});
  const toggleNode = (name: string) => setExpandedNodes(prev => ({ ...prev, [name]: !prev[name] }));

  const th: React.CSSProperties = {
    padding: '6px 10px', textAlign: 'left', fontSize: 11, fontWeight: 600,
    textTransform: 'uppercase', letterSpacing: '0.05em',
    color: T.textSubtle,
    borderBottom: `1px solid ${T.border}`,
    background: T.bgSecondary,
  };

  return (
    <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
      <colgroup>
        <col style={{ width: 28 }} />
        <col style={{ width: '30%' }} />
        <col style={{ width: 90 }} />
        <col style={{ width: 90 }} />
        <col style={{ width: 80 }} />
        <col style={{ width: 80 }} />
        <col />
      </colgroup>
      <thead>
        <tr>
          <th style={{ ...th, padding: '6px 4px' }} />
          <th style={th}>Node</th>
          <th style={th}>Phase</th>
          <th style={th}>Config</th>
          <th style={th}>In Phase</th>
          <th style={th}>Node Age</th>
          <th style={th}>Issues</th>
        </tr>
      </thead>
      <tbody>
        {nodes.map(node => {
          const nc = phaseColor(node.phase);
          const configMatch = node.currentConfig === node.desiredConfig;
          const hasBlockers = (node.drainBlockers?.length ?? 0) > 0;
          const hasProviderErrors = (node.providerErrors?.length ?? 0) > 0;
          const hasConditions = (node.conditions?.length ?? 0) > 0;
          const hasIssues = hasBlockers || hasProviderErrors;
          const hasDetail = hasBlockers || hasProviderErrors || hasConditions;
          const isExpanded = !!expandedNodes[node.name];
          const tdBase: React.CSSProperties = {
            padding: '7px 10px',
            borderBottom: isExpanded ? 'none' : `1px solid ${T.border}`,
            verticalAlign: 'top',
          };

          return (
            <React.Fragment key={node.name}>
              <tr
                onClick={() => hasDetail && toggleNode(node.name)}
                style={{
                  borderLeft: hasIssues ? `3px solid ${nc}` : '3px solid transparent',
                  cursor: hasDetail ? 'pointer' : 'default',
                }}
              >
                <td style={{ ...tdBase, padding: '7px 4px', textAlign: 'center', color: T.textMuted, fontSize: 11 }}>
                  {hasDetail ? (isExpanded ? '▾' : '▸') : ''}
                </td>
                <td style={{ ...tdBase, wordBreak: 'break-all' }}>
                  <div style={{ fontFamily: 'monospace', fontSize: 11 }}>{node.name}</div>
                  {node.roles && node.roles.length > 0 && (
                    <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap', marginTop: 3 }}>
                      {node.roles.map(role => (
                        <span key={role} style={{
                          fontSize: 10, fontWeight: 600, padding: '1px 6px',
                          borderRadius: T.radiusPill,
                          background: `${roleColor(role)}22`,
                          color: roleColor(role),
                          border: `1px solid ${roleColor(role)}55`,
                          textTransform: 'capitalize',
                        }}>
                          {role}
                        </span>
                      ))}
                    </div>
                  )}
                </td>
                <td style={tdBase}><PhaseBadge phase={node.phase} /></td>
                <td style={tdBase}>
                  {configMatch
                    ? <span style={{ color: T.success }}>✓ current</span>
                    : <span style={{ color: nc, fontFamily: 'monospace', fontSize: 11 }}>
                        →{(node.desiredConfig ?? '').slice(-8)}
                      </span>
                  }
                </td>
                {/* In Phase: how long this node has been in its current upgrade phase */}
                <td style={{ ...tdBase, color: T.textSubtle, whiteSpace: 'nowrap' }}>
                  {fmtElapsed(node.elapsedSeconds)}
                </td>
                {/* Node Age: time since node was created */}
                <td style={{ ...tdBase, color: T.textMuted, whiteSpace: 'nowrap' }}>
                  {fmtAge(node.ageSeconds)}
                </td>
                <td style={tdBase}>
                  {hasBlockers && (
                    <span style={{ color: T.danger, fontWeight: 600 }}>
                      {node.drainBlockers!.length} PDB blocker{node.drainBlockers!.length > 1 ? 's' : ''}
                    </span>
                  )}
                  {hasProviderErrors && (
                    <span style={{ color: T.warning, fontWeight: 600, marginLeft: hasBlockers ? 8 : 0 }}>
                      {node.providerErrors!.length} provider error{node.providerErrors!.length > 1 ? 's' : ''}
                    </span>
                  )}
                  {!hasIssues && <span style={{ color: T.textSubtle }}>—</span>}
                </td>
              </tr>

              {isExpanded && (
                <tr style={{ borderLeft: `3px solid ${nc}`, borderBottom: `1px solid ${T.border}` }}>
                  <td colSpan={7} style={{ padding: 0 }}>
                    <NodeDetailPanel node={node} borderColor={nc} />
                  </td>
                </tr>
              )}
            </React.Fragment>
          );
        })}
      </tbody>
    </table>
  );
};

// ---- Machine pool section --------------------------------------------------

const MachinePoolCard: React.FC<{ pool: MachinePoolStatus }> = ({ pool }) => {
  const [expanded, setExpanded] = React.useState(false);
  const color = phaseColor(pool.phase);

  const countStyle = (highlight?: boolean): React.CSSProperties => ({
    display: 'flex', flexDirection: 'column', alignItems: 'center',
    padding: '6px 14px',
    background: T.bgSecondary,
    borderRadius: T.radiusMd,
    border: `1px solid ${highlight ? color + '55' : T.border}`,
  });

  return (
    <div style={{ border: `1px solid ${T.border}`, borderRadius: T.radiusMd, overflow: 'hidden', marginBottom: 12 }}>
      {/* Pool header — fully clickable when nodes are present */}
      <div
        onClick={() => pool.nodes && pool.nodes.length > 0 && setExpanded(e => !e)}
        style={{
          display: 'flex', alignItems: 'center', gap: 12,
          padding: '10px 14px',
          background: T.bgSecondary,
          borderBottom: expanded ? `1px solid ${T.border}` : undefined,
          borderLeft: `4px solid ${color}`,
          cursor: pool.nodes && pool.nodes.length > 0 ? 'pointer' : 'default',
          userSelect: 'none',
        }}
      >
        <span style={{ fontWeight: 700, fontSize: 14 }}>{pool.name}</span>
        <PhaseBadge phase={pool.phase} size="sm" />
        <span style={{ marginLeft: 'auto', display: 'flex', gap: 8, alignItems: 'center' }}>
          {/* Count pills */}
          <span style={countStyle(pool.updated < pool.total)}>
            <span style={{ fontSize: 16, fontWeight: 700, color: pool.updated === pool.total ? T.success : T.info }}>
              {pool.updated}/{pool.total}
            </span>
            <span style={{ fontSize: 10, color: T.textSubtle }}>updated</span>
          </span>
          <span style={countStyle()}>
            <span style={{ fontSize: 16, fontWeight: 700 }}>{pool.ready}</span>
            <span style={{ fontSize: 10, color: T.textSubtle }}>ready</span>
          </span>
          {pool.unavailable > 0 && (
            <span style={countStyle(true)}>
              <span style={{ fontSize: 16, fontWeight: 700, color: T.warning }}>{pool.unavailable}</span>
              <span style={{ fontSize: 10, color: T.textSubtle }}>unavailable</span>
            </span>
          )}
          {pool.degraded > 0 && (
            <span style={countStyle(true)}>
              <span style={{ fontSize: 16, fontWeight: 700, color: T.danger }}>{pool.degraded}</span>
              <span style={{ fontSize: 10, color: T.textSubtle }}>degraded</span>
            </span>
          )}
          {pool.elapsedSeconds !== undefined && pool.elapsedSeconds > 0 && (
            <span style={{ fontSize: 12, color: T.textSubtle, marginLeft: 4 }}>
              {fmtElapsed(pool.elapsedSeconds)}
            </span>
          )}
          {pool.nodes && pool.nodes.length > 0 && (
            <span style={{ color: T.textSubtle, fontSize: 12, marginLeft: 4 }}>
              {expanded ? '▼' : '▶'} {pool.nodes.length} node{pool.nodes.length !== 1 ? 's' : ''}
            </span>
          )}
        </span>
      </div>

      {/* Node table */}
      {expanded && pool.nodes && pool.nodes.length > 0 && (
        <NodeTable nodes={pool.nodes} />
      )}
    </div>
  );
};

// ---- Collapsible operators card --------------------------------------------

const OperatorsCard: React.FC<{ ops: OperatorsStatus }> = ({ ops }) => {
  const [expanded, setExpanded] = React.useState(false);
  const color = phaseColor(ops.phase);

  const countPill = (n: number, label: string, c: string) => n > 0 ? (
    <span key={label} style={{
      display: 'inline-flex', alignItems: 'center', gap: 4,
      padding: '1px 8px', borderRadius: T.radiusPill,
      background: `${c}22`, color: c, border: `1px solid ${c}55`,
      fontWeight: 600, fontSize: 11,
    }}>
      {n} {label}
    </span>
  ) : null;

  return (
    <div style={{ border: `1px solid ${T.border}`, borderRadius: T.radiusLg, overflow: 'hidden', marginBottom: 20, boxShadow: T.shadow }}>
      {/* Clickable header row */}
      <div
        onClick={() => setExpanded(e => !e)}
        style={{
          display: 'flex', alignItems: 'center', gap: 10, cursor: 'pointer',
          padding: '10px 16px',
          background: T.bgSecondary,
          borderBottom: expanded ? `1px solid ${T.border}` : undefined,
          userSelect: 'none',
        }}
      >
        <span style={{ fontSize: 11, fontWeight: 700, textTransform: 'uppercase', letterSpacing: '0.07em', color: T.textSubtle }}>
          Cluster Operators
        </span>
        <PhaseBadge phase={ops.phase} />
        <span style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {countPill(ops.available,    'available',   T.success)}
          {countPill(ops.progressing,  'progressing', T.info)}
          {countPill(ops.degraded,     'degraded',    T.danger)}
        </span>
        {ops.elapsedSeconds ? (
          <span style={{ fontSize: 12, color: T.textSubtle, marginLeft: 4 }}>
            {fmtElapsed(ops.elapsedSeconds)} elapsed
          </span>
        ) : null}
        <span style={{ marginLeft: 'auto', color: T.textSubtle, fontSize: 12 }}>
          {ops.total} total · {expanded ? 'collapse ▲' : 'expand ▼'}
        </span>
      </div>

      {expanded && ops.operators && <OperatorsTable ops={ops} />}
    </div>
  );
};

// ---- MachineHealthCheck card -----------------------------------------------

const MachineHealthChecksCard: React.FC<{ mhcs: MachineHealthCheckStatus[] }> = ({ mhcs }) => {
  const [expanded, setExpanded] = React.useState(false);

  const hasIssues = mhcs.some(m => m.remediating || m.remediationsAllowed === 0 && m.expectedMachines > 0);

  const th: React.CSSProperties = {
    padding: '8px 12px', textAlign: 'left', fontSize: 11, fontWeight: 600,
    textTransform: 'uppercase', letterSpacing: '0.05em',
    color: T.textSubtle, borderBottom: `1px solid ${T.border}`,
    background: T.bgSecondary, whiteSpace: 'nowrap',
  };

  const mhcStatusLabel = (m: MachineHealthCheckStatus): { label: string; color: string } => {
    if (m.expectedMachines === 0) return { label: 'No machines', color: T.textSubtle };
    if (m.remediating && m.remediationsAllowed === 0) return { label: 'Paused — threshold hit', color: T.danger };
    if (m.remediating) return { label: 'Remediating', color: T.warning };
    return { label: 'Healthy', color: T.success };
  };

  return (
    <div style={{ border: `1px solid ${T.border}`, borderRadius: T.radiusLg, overflow: 'hidden', marginBottom: 20, boxShadow: T.shadow }}>
      <div
        onClick={() => setExpanded(e => !e)}
        style={{
          display: 'flex', alignItems: 'center', gap: 10, cursor: 'pointer',
          padding: '10px 16px',
          background: T.bgSecondary,
          borderBottom: expanded ? `1px solid ${T.border}` : undefined,
          userSelect: 'none',
        }}
      >
        <span style={{ fontSize: 11, fontWeight: 700, textTransform: 'uppercase', letterSpacing: '0.07em', color: T.textSubtle }}>
          Machine Health Checks
        </span>
        {hasIssues && (
          <span style={{
            display: 'inline-flex', alignItems: 'center', gap: 4,
            padding: '1px 8px', borderRadius: T.radiusPill,
            background: `${T.warning}22`, color: T.warning, border: `1px solid ${T.warning}55`,
            fontWeight: 600, fontSize: 11,
          }}>
            {mhcs.filter(m => m.remediating).length} remediating
          </span>
        )}
        <span style={{ marginLeft: 'auto', color: T.textSubtle, fontSize: 12 }}>
          {mhcs.length} total · {expanded ? 'collapse ▲' : 'expand ▼'}
        </span>
      </div>

      {expanded && (
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
          <thead>
            <tr>
              <th style={th}>Name</th>
              <th style={{ ...th, width: 110 }}>Max Unhealthy</th>
              <th style={{ ...th, width: 100 }}>Expected</th>
              <th style={{ ...th, width: 100 }}>Healthy</th>
              <th style={{ ...th, width: 100 }}>Allowed</th>
              <th style={th}>Status</th>
            </tr>
          </thead>
          <tbody>
            {mhcs.map(m => {
              const { label, color } = mhcStatusLabel(m);
              const unhealthy = m.expectedMachines - m.currentHealthy;
              const tdStyle: React.CSSProperties = {
                padding: '8px 12px', borderBottom: `1px solid ${T.border}`, verticalAlign: 'middle',
              };
              return (
                <tr key={m.name} style={{ borderLeft: m.remediating ? `3px solid ${color}` : '3px solid transparent' }}>
                  <td style={{ ...tdStyle, fontFamily: 'monospace', fontSize: 12 }}>{m.name}</td>
                  <td style={{ ...tdStyle, color: T.textSubtle }}>{m.maxUnhealthy || '—'}</td>
                  <td style={{ ...tdStyle, color: T.textSubtle }}>{m.expectedMachines}</td>
                  <td style={{ ...tdStyle }}>
                    <span style={{ color: unhealthy > 0 ? color : T.success, fontWeight: unhealthy > 0 ? 700 : 400 }}>
                      {m.currentHealthy}
                      {unhealthy > 0 && <span style={{ fontWeight: 400, color: T.textSubtle }}> / {m.expectedMachines}</span>}
                    </span>
                  </td>
                  <td style={{ ...tdStyle, color: m.remediationsAllowed === 0 && m.expectedMachines > 0 ? T.danger : T.textSubtle }}>
                    {m.remediationsAllowed}
                  </td>
                  <td style={{ ...tdStyle }}>
                    <span style={{
                      display: 'inline-flex', alignItems: 'center', gap: 4,
                      padding: '1px 8px', borderRadius: T.radiusPill, fontSize: 12, fontWeight: 600,
                      background: `${color}22`, color, border: `1px solid ${color}55`,
                    }}>
                      {label}
                    </span>
                    {m.remediating && m.remediationsAllowed === 0 && (
                      <div style={{ fontSize: 11, color: T.danger, marginTop: 3, fontStyle: 'italic' }}>
                        Remediation paused — unhealthy machines exceed MaxUnhealthy threshold.
                        New remediations will not start until machines recover.
                      </div>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
};

// ---- Phase stepper (Phase C) -----------------------------------------------

type StepperStatus = 'done' | 'active' | 'pending' | 'failed' | 'warning' | 'skipped';

// Phase order for upgrade progress. 'failed' is not in the sequence (handled separately).
const STEP_PHASE_ORDER = ['not-started', 'pre-checks', 'control-plane', 'operators', 'worker-upgrade', 'complete'];


// Context type describing how relevant a preflight check snapshot is relative to the current
// upgrade state. Used to drive banner wording and re-run suggestions.
type CheckContext =
  | 'pre-upgrade-snapshot' // check ran before the current upgrade started — ideal
  | 'during-upgrade'       // check ran after the upgrade started (less representative)
  | 'fresh'                // <24h old, no active upgrade
  | 'aging'                // 1–7d old, consider re-running
  | 'stale'                // 7–30d old, cluster state likely changed
  | 'very-stale';          // >30d old, strongly recommend re-run

function getStepStatus(stationPhase: string, currentPhase: string, isFailed: boolean): StepperStatus {
  const stationIdx = STEP_PHASE_ORDER.indexOf(stationPhase);
  const currentIdx = STEP_PHASE_ORDER.indexOf(currentPhase);
  if (stationIdx < 0) return 'pending';
  // When upgrade has failed, treat the station just before 'failed' as the failed station.
  // We approximate by using the last active phase from snapshot.
  if (isFailed) {
    // All phases with index < currentIdx are done; currentIdx is failed.
    if (stationIdx < currentIdx) return 'done';
    if (stationIdx === currentIdx) return 'failed';
    return 'pending';
  }
  if (currentIdx < 0) return 'pending';
  if (stationIdx < currentIdx) return 'done';
  if (stationIdx === currentIdx) return 'active';
  return 'pending';
}

const stepStatusColor = (s: StepperStatus): string => {
  if (s === 'done')    return T.success;
  if (s === 'active')  return T.info;
  if (s === 'failed')  return T.danger;
  if (s === 'warning') return T.warning;
  return T.textSubtle;
};

// Traffic-light style icon: solid filled circle for active/done/failed, dashed outline for pending.
const StepIcon: React.FC<{ status: StepperStatus }> = ({ status }) => {
  const color = stepStatusColor(status);
  const isPending = status === 'pending' || status === 'skipped';
  const size = 40;
  return (
    <svg width={size} height={size} viewBox="0 0 40 40" aria-hidden="true" style={{ flexShrink: 0 }}>
      {/* Background fill */}
      <circle cx={20} cy={20} r={17}
        fill={isPending ? 'transparent' : color}
        stroke={color} strokeWidth={isPending ? 2.5 : 0}
        strokeDasharray={isPending ? '5 3' : undefined}
        opacity={isPending ? 0.7 : 1}
      />
      {/* Center dot for pending */}
      {isPending && <circle cx={20} cy={20} r={5} fill={color} opacity={0.45} />}
      {/* Checkmark for done */}
      {status === 'done' && (
        <path d="M11 20 L17 26.5 L29 13.5" fill="none" stroke="white" strokeWidth={3.5}
          strokeLinecap="round" strokeLinejoin="round" />
      )}
      {/* Dot for active */}
      {status === 'active' && <circle cx={20} cy={20} r={8} fill="white" opacity={0.9} />}
      {/* X for failed */}
      {status === 'failed' && (
        <path d="M13 13 L27 27 M27 13 L13 27" fill="none" stroke="white"
          strokeWidth={3.5} strokeLinecap="round" />
      )}
      {/* ! for warning */}
      {status === 'warning' && (
        <text x="20" y="27" textAnchor="middle" fill="white" fontSize="19" fontWeight="700">!</text>
      )}
    </svg>
  );
};

// Pre-checks detail panel (shown when the Pre-checks station is expanded in the stepper).
const PreChecksStationDetail: React.FC<{
  matchingCheck: UpgradeDiagsCheck | null;
  preflightIssues: DiagsCheckResult[];
  onRunCheckRequested?: (version: string, channel: string) => void;
  targetVersion?: string;
  targetChannel?: string;
}> = ({ matchingCheck, preflightIssues, onRunCheckRequested, targetVersion, targetChannel }) => {
  if (!matchingCheck) {
    const hasTarget = !!targetVersion;
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <div style={{ flex: 1, fontSize: 13, color: T.textSubtle }}>
          {hasTarget
            ? <>No diagnostics check found for <strong>{targetVersion}</strong>. Run a check to identify potential roadblocks before or during this upgrade.</>
            : <>No upgrade is currently scheduled. Run a diagnostics check at any time to validate cluster readiness before planning an upgrade.</>
          }
        </div>
        {onRunCheckRequested && targetVersion && (
          <Button
            variant="primary" size="sm"
            onClick={() => onRunCheckRequested(targetVersion, targetChannel ?? '')}
            style={{ borderRadius: T.radiusMd, whiteSpace: 'nowrap' }}
          >
            Run Check for {targetVersion}
          </Button>
        )}
      </div>
    );
  }

  const s = matchingCheck.status;
  const totalChecks = s?.checks?.length ?? 0;
  const passed  = s?.checksPassed  ?? 0;
  const failed  = s?.checksFailed  ?? 0;
  const warned  = s?.checksWarned  ?? 0;
  const errored = s?.checksErrored ?? 0;

  return (
    <div>
      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', alignItems: 'center', marginBottom: preflightIssues.length > 0 ? 10 : 0 }}>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
          {([
            { n: passed,  label: 'passed',  color: T.success },
            { n: failed,  label: 'failed',  color: T.danger },
            { n: warned,  label: 'warned',  color: T.warning },
            { n: errored, label: 'error',   color: T.warning },
          ] as Array<{ n: number; label: string; color: string }>).map(({ n, label, color }) => n > 0 ? (
            <span key={label} style={{
              display: 'inline-flex', alignItems: 'center', gap: 4,
              padding: '2px 10px', borderRadius: T.radiusPill, fontWeight: 600, fontSize: 12,
              background: `${color}22`, color, border: `1px solid ${color}55`,
            }}>
              {n} {label}
            </span>
          ) : null)}
          <span style={{ fontSize: 12, color: T.textMuted }}>{totalChecks} total checks</span>
        </div>
        <span style={{ fontSize: 12, color: T.textSubtle, marginLeft: 'auto' }}>
          See <strong>Diagnostics Checks</strong> tab for full details
        </span>
      </div>
      {preflightIssues.length > 0 && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
          {preflightIssues.slice(0, 5).map(c => {
            const color = c.result === 'Failed' || c.result === 'Error' ? T.danger : T.warning;
            return (
              <div key={c.name} style={{
                display: 'flex', alignItems: 'flex-start', gap: 8,
                padding: '4px 8px', borderRadius: T.radiusSm,
                background: `${color}11`, borderLeft: `3px solid ${color}`, fontSize: 12,
              }}>
                <span style={{ color, fontWeight: 700, minWidth: 50 }}>{c.result}</span>
                <span style={{ fontWeight: 600, color: T.textSubtle }}>
                  {c.name.replace(/([a-z])([A-Z])/g, '$1 $2')}
                </span>
                {c.execution?.summary && (
                  <span style={{ color: T.textMuted, flex: 1 }}>— {c.execution.summary}</span>
                )}
              </div>
            );
          })}
          {preflightIssues.length > 5 && (
            <div style={{ fontSize: 12, color: T.textMuted, padding: '2px 8px' }}>
              +{preflightIssues.length - 5} more — see Diagnostics Checks tab
            </div>
          )}
        </div>
      )}
    </div>
  );
};

interface StepStation {
  id: string;
  label: string;
  status: StepperStatus;
  elapsed?: number;
  summary?: string;
}

// Small inline block shown inside a stepper station detail panel when preflight
// findings map to that phase. Helps users connect pre-upgrade warnings to
// what's happening now.
const PreCheckFindingsCallout: React.FC<{ findings: DiagsCheckResult[]; checkContext: CheckContext | null }> = ({ findings, checkContext }) => {
  if (findings.length === 0) return null;
  const hasErrors = findings.some(c => c.result === 'Failed' || c.result === 'Error');
  const accent = hasErrors ? T.danger : T.warning;
  const contextLabel = checkContext === 'pre-upgrade-snapshot'
    ? 'Pre-upgrade check'
    : checkContext === 'during-upgrade'
    ? 'Check (during upgrade)'
    : 'Pre-flight check';
  return (
    <div style={{
      marginTop: 12, padding: '8px 12px',
      background: `${accent}10`,
      border: `1px solid ${accent}44`,
      borderLeft: `3px solid ${accent}`,
      borderRadius: T.radiusSm,
    }}>
      <div style={{ fontSize: 11, fontWeight: 700, color: accent, textTransform: 'uppercase',
        letterSpacing: '0.06em', marginBottom: 6 }}>
        {contextLabel} findings for this phase
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
        {findings.map(c => {
          const fc = c.result === 'Failed' || c.result === 'Error' ? T.danger : T.warning;
          return (
            <div key={c.name} style={{ display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 12 }}>
              <span style={{
                flexShrink: 0, fontSize: 10, fontWeight: 700, padding: '1px 6px',
                borderRadius: T.radiusPill, background: `${fc}22`, color: fc,
                border: `1px solid ${fc}55`,
              }}>
                {c.result}
              </span>
              <span style={{ color: T.textSubtle }}>
                {c.name.replace(/([a-z])([A-Z])/g, '$1 $2')}
              </span>
              {c.execution?.summary && (
                <span style={{ color: T.textMuted, fontSize: 11 }}>— {c.execution.summary}</span>
              )}
            </div>
          );
        })}
      </div>
      <div style={{ fontSize: 11, color: T.textMuted, marginTop: 6 }}>
        See <strong>Diagnostics Checks</strong> tab for details and remediation steps.
      </div>
    </div>
  );
};

const UpgradePhaseStepper: React.FC<{
  snapshot: UpgradeSnapshot;
  matchingCheck: UpgradeDiagsCheck | null;
  preflightIssues: DiagsCheckResult[];
  checkContext: CheckContext | null;
  onRunCheckRequested?: (version: string, channel: string) => void;
}> = ({ snapshot, matchingCheck, preflightIssues, checkContext, onRunCheckRequested }) => {
  const isFailed = snapshot.phase === 'failed';

  // Returns preflight issues that map to a specific stepper station.
  // The backend populates c.upgradePhase from each check's UpgradePhase() method,
  // so new checks are automatically correlated without any frontend changes.
  const findingsForStation = (stationId: string) =>
    preflightIssues.filter(c => c.upgradePhase === stationId);

  // Determine the "effective current phase" for getStepStatus when phase=failed.
  // We approximate: use the last phase that would have been active before failure.
  // The API may expose controlPlane.phase etc. as clues, but we default to 'worker-upgrade'.
  const effectivePhase = isFailed
    ? (snapshot.clusterOperators?.phase === 'complete' ? 'worker-upgrade'
      : snapshot.controlPlane?.phase === 'complete' ? 'operators'
      : 'control-plane')
    : snapshot.phase;

  // Auto-open the most relevant station on mount.
  // not-started → pre-checks (most actionable when no upgrade is running)
  // active phase → that phase's station
  // failed → the station that was active when it failed
  const [selectedStation, setSelectedStation] = React.useState<string | null>(() => {
    if (snapshot.phase === 'not-started') return 'pre-checks';
    if (snapshot.phase === 'failed') return effectivePhase;
    if (STEP_PHASE_ORDER.includes(snapshot.phase) && snapshot.phase !== 'complete') {
      return snapshot.phase;
    }
    return null;
  });

  // Worker aggregate counts
  const totalWorkerNodes   = (snapshot.machinePools ?? []).reduce((a, p) => a + p.total, 0);
  const updatedWorkerNodes = (snapshot.machinePools ?? []).reduce((a, p) => a + p.updated, 0);
  const notStarted = snapshot.phase === 'not-started';

  const stations: StepStation[] = [
    {
      id:      'pre-checks',
      label:   'Pre-checks',
      status:  getStepStatus('pre-checks', effectivePhase, isFailed),
      elapsed: (() => {
        const s = matchingCheck?.status;
        if (!s?.startedAt) return undefined;
        const start = new Date(s.startedAt).getTime();
        const end   = s.completedAt ? new Date(s.completedAt).getTime() : Date.now();
        return Math.round((end - start) / 1000);
      })(),
      summary: !matchingCheck
        ? 'No check run'
        : `${matchingCheck.status?.checksPassed ?? 0} passed` +
          (preflightIssues.length > 0 ? `, ${preflightIssues.length} issue${preflightIssues.length !== 1 ? 's' : ''}` : ''),
    },
    {
      id:      'control-plane',
      label:   'Control Plane',
      status:  getStepStatus('control-plane', effectivePhase, isFailed),
      elapsed: snapshot.controlPlane?.elapsedSeconds,
      // In not-started state avoid showing "Not Started" — that's noise.
      summary: notStarted ? '—' : (snapshot.controlPlane?.phase ? phaseLabel(snapshot.controlPlane.phase) : '—'),
    },
    {
      id:      'operators',
      label:   'Operators',
      status:  getStepStatus('operators', effectivePhase, isFailed),
      elapsed: snapshot.clusterOperators?.elapsedSeconds,
      // "available" describes current health; "updated" implies upgrade completion — use the
      // right word based on context so the stepper doesn't look like an upgrade already ran.
      summary: snapshot.clusterOperators
        ? notStarted
          ? `${snapshot.clusterOperators.available} available`
          : `${snapshot.clusterOperators.available}/${snapshot.clusterOperators.total} upgraded`
        : '—',
    },
    {
      id:      'worker-upgrade',
      label:   'Workers',
      status:  getStepStatus('worker-upgrade', effectivePhase, isFailed),
      elapsed: (snapshot.machinePools ?? []).reduce<number | undefined>(
        (max, p) => p.elapsedSeconds !== undefined ? Math.max(max ?? 0, p.elapsedSeconds) : max, undefined
      ),
      summary: totalWorkerNodes > 0
        ? notStarted
          ? `${totalWorkerNodes} node${totalWorkerNodes !== 1 ? 's' : ''}`
          : `${updatedWorkerNodes}/${totalWorkerNodes} updated`
        : '—',
    },
    {
      id:      'complete',
      label:   'Complete',
      status:  snapshot.phase === 'complete' ? 'done' : isFailed ? 'failed' : 'pending',
      elapsed: snapshot.phase === 'complete' ? snapshot.elapsedSeconds : undefined,
      summary: snapshot.phase === 'complete' ? `Total ${fmtElapsed(snapshot.elapsedSeconds)}` : '—',
    },
  ];

  const toggleStation = (id: string) =>
    setSelectedStation(prev => prev === id ? null : id);

  const [hoveredStation, setHoveredStation] = React.useState<string | null>(null);

  return (

    <div style={{
      border: `1px solid ${T.border}`,
      borderRadius: T.radiusLg,
      overflow: 'hidden',
      boxShadow: T.shadow,
      marginBottom: 20,
    }}>
      <SectionHeader
        title="Upgrade Progress"
        badge={snapshot.phase !== 'not-started' ? <PhaseBadge phase={snapshot.phase} size="sm" /> : undefined}
        meta={
          snapshot.phase === 'not-started'
            ? 'No upgrade in progress'
            : snapshot.elapsedSeconds && snapshot.elapsedSeconds > 0
            ? `${fmtElapsed(snapshot.elapsedSeconds)} elapsed`
            : undefined
        }
      />

      {/* Station row */}
      <div style={{ padding: '16px 12px 12px', display: 'flex', alignItems: 'flex-start', gap: 2 }}>
        {stations.map((station, idx) => {
          const isSelected = selectedStation === station.id;
          const isHovered = hoveredStation === station.id;
          const color = stepStatusColor(station.status);
          // Connector turns green once the station on the left is complete.
          const connectorColor = station.status === 'done' ? T.success : T.border;

          return (
            <React.Fragment key={station.id}>
              {/* Station card */}
              <div
                onClick={() => toggleStation(station.id)}
                onMouseEnter={() => setHoveredStation(station.id)}
                onMouseLeave={() => setHoveredStation(null)}
                role="button"
                aria-expanded={isSelected}
                aria-label={`${station.label}: ${station.status}`}
                style={{
                  display: 'flex', flexDirection: 'column', alignItems: 'center',
                  flex: 1, minWidth: 80, cursor: 'pointer',
                  padding: '10px 6px 8px',
                  borderRadius: T.radiusMd,
                  // Card-style background — slightly raised, more visible hover/selected states
                  background: isSelected
                    ? `${color}20`
                    : isHovered
                    ? `${color}0f`
                    : 'var(--pf-t--global--background--color--secondary--default, rgba(0,0,0,0.05))',
                  border: `1px solid ${isSelected ? color : isHovered ? `${color}88` : T.border}`,
                  boxShadow: isSelected
                    ? `0 0 0 2px ${color}44, 0 2px 6px ${color}22`
                    : isHovered
                    ? `0 2px 8px ${color}30`
                    : '0 1px 3px rgba(0,0,0,0.12)',
                  transition: 'background 0.15s, border-color 0.15s, box-shadow 0.15s',
                  userSelect: 'none',
                }}
              >
                <StepIcon status={station.status} />
                <div style={{
                  marginTop: 7, fontSize: 11, fontWeight: 700, textAlign: 'center',
                  textTransform: 'uppercase', letterSpacing: '0.05em', color,
                }}>
                  {station.label}
                </div>
                {station.elapsed !== undefined && station.elapsed > 0 && (
                  <div style={{ fontSize: 11, color: T.textMuted, marginTop: 2 }}>
                    {fmtElapsed(station.elapsed)}
                  </div>
                )}
                {station.summary && (
                  <div style={{
                    fontSize: 10, color: T.textSubtle, marginTop: 2,
                    textAlign: 'center', lineHeight: 1.3, maxWidth: 110,
                  }}>
                    {station.summary}
                  </div>
                )}
                {/* Expand/collapse indicator */}
                <div style={{
                  marginTop: 5, fontSize: 10, color: isSelected ? color : T.textMuted,
                  fontWeight: isSelected ? 700 : 400,
                }}>
                  {isSelected ? '▲' : '▼'}
                </div>
              </div>

              {/* Connector: line + SVG chevron arrow */}
              {idx < stations.length - 1 && (
                <div style={{
                  flexShrink: 0, display: 'flex', alignItems: 'center',
                  alignSelf: 'flex-start', marginTop: 20, width: 28,
                }}>
                  <div style={{ flex: 1, height: 2, background: connectorColor }} />
                  <svg width={10} height={14} viewBox="0 0 10 14" style={{ flexShrink: 0, marginLeft: -1 }}
                    aria-hidden="true">
                    <path d="M1 1 L9 7 L1 13" fill="none" stroke={connectorColor}
                      strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" />
                  </svg>
                </div>
              )}
            </React.Fragment>
          );
        })}
      </div>

      {/* Expandable detail panel for selected station */}
      {selectedStation && (() => {
        const station = stations.find(s => s.id === selectedStation);
        if (!station) return null;
        const color = stepStatusColor(station.status);

        return (
          <div style={{
            borderTop: `1px solid ${T.border}`,
            padding: '14px 20px 16px',
            background: `${color}14`,
            borderLeft: `4px solid ${color}`,
          }}>
            {selectedStation === 'pre-checks' && (
              <>
                <div style={{ fontSize: 12, fontWeight: 700, color: T.textSubtle, textTransform: 'uppercase',
                  letterSpacing: '0.07em', marginBottom: 8 }}>
                  Preflight Diagnostics
                </div>
                <PreChecksStationDetail
                  matchingCheck={matchingCheck}
                  preflightIssues={preflightIssues}
                  onRunCheckRequested={onRunCheckRequested}
                  targetVersion={snapshot.targetVersion}
                  targetChannel={snapshot.targetChannel}
                />
              </>
            )}

            {selectedStation === 'control-plane' && (
              <>
                <div style={{ fontSize: 12, fontWeight: 700, color: T.textSubtle, textTransform: 'uppercase',
                  letterSpacing: '0.07em', marginBottom: 8 }}>
                  Control Plane
                </div>
                <div style={{ display: 'flex', gap: 24, flexWrap: 'wrap', alignItems: 'flex-start' }}>
                  <div>
                    <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase',
                      letterSpacing: '0.05em', marginBottom: 2 }}>Phase</div>
                    <PhaseBadge phase={snapshot.controlPlane?.phase ?? 'unknown'} size="sm" />
                  </div>
                  {snapshot.controlPlane?.elapsedSeconds !== undefined && (
                    <div>
                      <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase',
                        letterSpacing: '0.05em', marginBottom: 2 }}>Elapsed</div>
                      <div style={{ fontSize: 15, fontWeight: 600 }}>
                        {fmtElapsed(snapshot.controlPlane.elapsedSeconds)}
                      </div>
                    </div>
                  )}
                  {snapshot.controlPlane?.startedAt && (
                    <div>
                      <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase',
                        letterSpacing: '0.05em', marginBottom: 2 }}>Started</div>
                      <div style={{ fontSize: 12 }}>{new Date(snapshot.controlPlane.startedAt).toLocaleString()}</div>
                    </div>
                  )}
                  {snapshot.controlPlane?.message && (
                    <div style={{ flex: 1 }}>
                      <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase',
                        letterSpacing: '0.05em', marginBottom: 2 }}>Message</div>
                      <div style={{ fontSize: 12, color: T.textSubtle }}>{snapshot.controlPlane.message}</div>
                    </div>
                  )}
                </div>
                <PreCheckFindingsCallout findings={findingsForStation('control-plane')} checkContext={checkContext} />
              </>
            )}

            {selectedStation === 'operators' && snapshot.clusterOperators && (
              <>
                <div style={{ fontSize: 12, fontWeight: 700, color: T.textSubtle, textTransform: 'uppercase',
                  letterSpacing: '0.07em', marginBottom: 8 }}>
                  Cluster Operators
                </div>
                <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', alignItems: 'center' }}>
                  <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                    {([
                      { n: snapshot.clusterOperators.available,   label: 'available',   c: T.success },
                      { n: snapshot.clusterOperators.progressing, label: 'progressing', c: T.info },
                      { n: snapshot.clusterOperators.degraded,    label: 'degraded',    c: T.danger },
                    ] as Array<{ n: number; label: string; c: string }>).map(({ n, label, c }) => n > 0 ? (
                      <span key={label} style={{
                        display: 'inline-flex', alignItems: 'center', gap: 4,
                        padding: '2px 10px', borderRadius: T.radiusPill, fontWeight: 600, fontSize: 12,
                        background: `${c}22`, color: c, border: `1px solid ${c}55`,
                      }}>
                        {n} {label}
                      </span>
                    ) : null)}
                    <span style={{ fontSize: 12, color: T.textMuted }}>
                      {snapshot.clusterOperators.total} total
                    </span>
                  </div>
                  {snapshot.clusterOperators.elapsedSeconds !== undefined && (
                    <span style={{ fontSize: 12, color: T.textSubtle }}>
                      {fmtElapsed(snapshot.clusterOperators.elapsedSeconds)} elapsed
                    </span>
                  )}
                  <span style={{ fontSize: 12, color: T.textSubtle, marginLeft: 'auto' }}>
                    ↓ Expand Cluster Operators below for per-operator details
                  </span>
                </div>
                <PreCheckFindingsCallout findings={findingsForStation('operators')} checkContext={checkContext} />
              </>
            )}

            {selectedStation === 'worker-upgrade' && (
              <>
                <div style={{ fontSize: 12, fontWeight: 700, color: T.textSubtle, textTransform: 'uppercase',
                  letterSpacing: '0.07em', marginBottom: 8 }}>
                  Worker Node Pools
                </div>
                <div style={{ display: 'flex', gap: 10, flexWrap: 'wrap', alignItems: 'flex-start' }}>
                  {(snapshot.machinePools ?? []).map(pool => {
                    const pc = phaseColor(pool.phase);
                    return (
                      <div key={pool.name} style={{
                        display: 'flex', flexDirection: 'column', gap: 4,
                        padding: '8px 12px', border: `1px solid ${pc}44`,
                        borderRadius: T.radiusMd, minWidth: 110,
                      }}>
                        <div style={{ fontSize: 12, fontWeight: 700, color: pc }}>{pool.name}</div>
                        <div style={{ fontSize: 15, fontWeight: 600 }}>
                          {pool.updated}/{pool.total}
                          <span style={{ fontSize: 11, fontWeight: 400, color: T.textSubtle, marginLeft: 4 }}>updated</span>
                        </div>
                        <PhaseBadge phase={pool.phase} size="sm" />
                        {pool.elapsedSeconds !== undefined && pool.elapsedSeconds > 0 && (
                          <div style={{ fontSize: 11, color: T.textSubtle }}>{fmtElapsed(pool.elapsedSeconds)}</div>
                        )}
                        {pool.unavailable > 0 && (
                          <div style={{ fontSize: 11, color: T.warning }}>{pool.unavailable} unavailable</div>
                        )}
                        {pool.degraded > 0 && (
                          <div style={{ fontSize: 11, color: T.danger }}>{pool.degraded} degraded</div>
                        )}
                      </div>
                    );
                  })}
                  <span style={{ fontSize: 12, color: T.textSubtle, marginLeft: 'auto', alignSelf: 'center' }}>
                    ↓ Expand Machine Config Pools below for node-level details
                  </span>
                </div>
                <PreCheckFindingsCallout findings={findingsForStation('worker-upgrade')} checkContext={checkContext} />
              </>
            )}

            {selectedStation === 'complete' && (
              <div style={{ fontSize: 13 }}>
                {snapshot.phase === 'complete'
                  ? <span style={{ color: T.success, fontWeight: 700 }}>
                      ✓ Upgrade completed successfully in {fmtElapsed(snapshot.elapsedSeconds)}
                    </span>
                  : isFailed
                  ? <span style={{ color: T.danger, fontWeight: 700 }}>
                      Upgrade failed — review blockers and cluster operator status above
                    </span>
                  : <span style={{ color: T.textMuted }}>
                      Waiting for all upgrade phases to complete…
                    </span>
                }
              </div>
            )}
          </div>
        );
      })()}
    </div>
  );
};

// ---- Main component --------------------------------------------------------

interface UpgradeStatusViewProps {
  onRunCheckRequested?: (version: string, channel: string) => void;
}

export const UpgradeStatusView: React.FC<UpgradeStatusViewProps> = ({ onRunCheckRequested }) => {
  const [snapshot, setSnapshot]     = React.useState<UpgradeSnapshot | null>(null);
  const [loading, setLoading]       = React.useState(true);
  const [error, setError]           = React.useState<string | null>(null);
  const [lastRefresh, setLastRefresh] = React.useState<Date | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);
  const [history, setHistory]       = React.useState<UpgradeHistoryResponse | null>(null);
  const [muoVersion, setMuoVersion] = React.useState<{ version: string; gitCommit: string } | null>(null);

  // Watch UpgradeDiagsCheck CRs to surface preflight results for the current upgrade target.
  const [diagsChecks] = useK8sWatchResource<UpgradeDiagsCheck[]>({
    groupVersionKind: { group: 'upgrade.managed.openshift.io', version: 'v1alpha1', kind: 'UpgradeDiagsCheck' },
    namespace: MUO_NS,
    isList: true,
  });

  // Find the most recent check that matches the current upgrade target version.
  const matchingCheck = React.useMemo(() => {
    if (!Array.isArray(diagsChecks) || !snapshot?.targetVersion) return null;
    return [...diagsChecks]
      .filter(c => c.spec?.desired?.version === snapshot.targetVersion)
      .sort((a, b) => (b.metadata?.creationTimestamp ?? '').localeCompare(a.metadata?.creationTimestamp ?? ''))
      [0] ?? null;
  }, [diagsChecks, snapshot?.targetVersion]);

  const preflightIssues = matchingCheck?.status?.checks?.filter(
    c => c.result === 'Failed' || c.result === 'Error' || c.result === 'Warning'
  ) ?? [];

  // Age of the check in ms (from completedAt, falling back to startedAt if still running).
  const checkAgeMs = React.useMemo(() => {
    const ts = matchingCheck?.status?.completedAt ?? matchingCheck?.status?.startedAt;
    return ts ? Date.now() - new Date(ts).getTime() : null;
  }, [matchingCheck]);
  const checkIsStale = checkAgeMs !== null && checkAgeMs > 24 * 60 * 60 * 1000;

  // Classify how relevant the preflight check is relative to the current upgrade state.
  // 'pre-upgrade-snapshot': check ran before the upgrade started — the ideal use case.
  // 'during-upgrade':       check ran after the upgrade started (less representative).
  // 'fresh/aging/stale/very-stale': no active upgrade; keyed on absolute age.
  const checkContext: CheckContext | null = React.useMemo(() => {
    if (!matchingCheck) return null;
    const isActive = !!snapshot && snapshot.phase !== 'not-started' && snapshot.phase !== 'complete';
    if (isActive && checkAgeMs !== null) {
      // Estimate when the upgrade started: controlPlane.startedAt if available, else now - elapsedSeconds.
      const upgradeStartMs = snapshot?.controlPlane?.startedAt
        ? new Date(snapshot.controlPlane.startedAt).getTime()
        : snapshot?.elapsedSeconds
        ? Date.now() - snapshot.elapsedSeconds * 1000
        : null;
      if (upgradeStartMs !== null) {
        const checkCompletedMs = Date.now() - checkAgeMs;
        return checkCompletedMs < upgradeStartMs ? 'pre-upgrade-snapshot' : 'during-upgrade';
      }
    }
    if (checkAgeMs === null) return null;
    if (checkAgeMs < 24 * 3_600_000)   return 'fresh';      // <24h
    if (checkAgeMs < 7 * 86_400_000)   return 'aging';      // 1–7d
    if (checkAgeMs < 30 * 86_400_000)  return 'stale';      // 7–30d
    return 'very-stale';                                      // >30d
  }, [matchingCheck, snapshot, checkAgeMs]);

  const fmtAge = (ms: number) => {
    const mins = Math.floor(ms / 60_000);
    if (ms < 3_600_000)  return `${mins} min${mins !== 1 ? 's' : ''}`;
    const hrs = Math.floor(ms / 3_600_000);
    if (ms < 86_400_000) return `${hrs} hr${hrs !== 1 ? 's' : ''}`;
    const days = Math.floor(ms / 86_400_000);
    return `${days} day${days !== 1 ? 's' : ''}`;
  };

  const fetchStatus = React.useCallback(async (manual = false) => {
    if (manual) setRefreshing(true);
    try {
      const resp = await consoleFetch(STATUS_API);
      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}: ${resp.statusText}`);
      }
      const data: UpgradeSnapshot = await resp.json();
      setSnapshot(data);
      setError(null);
      setLastRefresh(new Date());
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to fetch upgrade status');
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  const fetchHistory = React.useCallback(async () => {
    try {
      const resp = await consoleFetch(HISTORY_API);
      if (!resp.ok) return;
      const data: UpgradeHistoryResponse = await resp.json();
      setHistory(data);
    } catch {
      // history is best-effort — don't surface an error for it
    }
  }, []);

  const fetchVersion = React.useCallback(async () => {
    try {
      const resp = await consoleFetch(VERSION_API);
      if (!resp.ok) return;
      const data: { version: string; gitCommit: string } = await resp.json();
      setMuoVersion(data);
    } catch {
      // version is best-effort
    }
  }, []);

  React.useEffect(() => {
    fetchStatus();
    const interval = setInterval(() => fetchStatus(), 30_000);
    return () => clearInterval(interval);
  }, [fetchStatus]);

  React.useEffect(() => {
    fetchHistory();
  }, [fetchHistory]);

  React.useEffect(() => {
    fetchVersion();
  }, [fetchVersion]);

  const cardStyle: React.CSSProperties = {
    border: `1px solid ${T.border}`,
    borderRadius: T.radiusLg,
    overflow: 'hidden',
    boxShadow: T.shadow,
    marginBottom: 20,
  };

  if (loading && !snapshot) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
        <Spinner aria-label="Loading upgrade status" />
      </div>
    );
  }

  return (
    <div>
      {/* Toolbar */}
      <div style={{ display: 'flex', alignItems: 'center', marginBottom: 16, gap: 10 }}>
        <Button
          variant="secondary"
          icon={<SyncAltIcon />}
          onClick={() => fetchStatus(true)}
          isDisabled={refreshing}
          isLoading={refreshing}
          style={{ borderRadius: T.radiusMd }}
        >
          Refresh
        </Button>
        {lastRefresh && (
          <span style={{ fontSize: 12, color: T.textSubtle }}>
            Last refreshed {lastRefresh.toLocaleTimeString()}
          </span>
        )}
        {muoVersion && (
          <span style={{
            marginLeft: 'auto', fontSize: 11, color: T.textMuted,
            fontFamily: 'monospace', letterSpacing: '0.03em',
          }}
            title={`MUO version ${muoVersion.version} · commit ${muoVersion.gitCommit}`}
          >
            MUO {muoVersion.gitCommit === 'unknown' ? muoVersion.version : muoVersion.gitCommit.slice(0, 7)}
          </span>
        )}
      </div>

      {error && (
        <Alert variant="danger" isInline title="Failed to load upgrade status" style={{ marginBottom: 16 }}>
          {error}
        </Alert>
      )}

      {snapshot && (
        <>
          {/* ---- Phase stepper (Phase C) ---- */}
          <UpgradePhaseStepper
            snapshot={snapshot}
            matchingCheck={matchingCheck}
            preflightIssues={preflightIssues}
            checkContext={checkContext}
            onRunCheckRequested={onRunCheckRequested}
          />

          {/* ---- Summary card ---- */}
          <div style={cardStyle}>
            <SectionHeader
              title="Cluster Upgrade"
              badge={snapshot.phase !== 'not-started' ? <PhaseBadge phase={snapshot.phase} size="md" /> : undefined}
            />
            <div style={{ padding: '14px 16px', display: 'flex', gap: 24, flexWrap: 'wrap', alignItems: 'flex-start' }}>
              {/* Current version */}
              <div>
                <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 4 }}>
                  Current Version
                </div>
                <div style={{ fontSize: 20, fontWeight: 700, fontFamily: 'monospace' }}>
                  {snapshot.currentVersion ?? '—'}
                </div>
              </div>

              {/* Active upgrade: show target + elapsed */}
              {snapshot.targetVersion && snapshot.targetVersion !== snapshot.currentVersion && (<>
                <div style={{ alignSelf: 'center', color: T.textSubtle, fontSize: 20 }}>→</div>
                <div>
                  <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 4 }}>
                    Target Version
                  </div>
                  <div style={{ fontSize: 20, fontWeight: 700, fontFamily: 'monospace', color: T.info }}>
                    {snapshot.targetVersion}
                  </div>
                </div>
                {snapshot.elapsedSeconds !== undefined && snapshot.elapsedSeconds > 0 && (
                  <div>
                    <div style={{ fontSize: 11, color: T.textSubtle, textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 4 }}>
                      Elapsed
                    </div>
                    <div style={{ fontSize: 18, fontWeight: 600 }}>
                      {fmtElapsed(snapshot.elapsedSeconds)}
                    </div>
                  </div>
                )}
              </>)}

              {/* Not-started: explain state so the card isn't just an empty version block */}
              {snapshot.phase === 'not-started' && (
                <div style={{ alignSelf: 'center', fontSize: 13, color: T.textMuted, fontStyle: 'italic' }}>
                  Cluster is at the current version — no upgrade scheduled.
                </div>
              )}
            </div>
            {snapshot.cvoMessage && (
              <div style={{
                padding: '8px 16px',
                borderTop: `1px solid ${T.border}`,
                fontSize: 12, color: T.textSubtle, fontStyle: 'italic',
              }}>
                {snapshot.cvoMessage}
              </div>
            )}
          </div>

          {/* ---- Blockers ---- */}
          {snapshot.blockers && snapshot.blockers.length > 0 && (
            <Alert variant="danger" isInline title={`${snapshot.blockers.length} upgrade blocker(s) detected`} style={{ marginBottom: 20 }}>
              <ul style={{ margin: 0, paddingLeft: 20 }}>
                {snapshot.blockers.map((b, i) => (
                  <li key={i}>
                    <strong>{b.component}</strong>
                    {b.elapsedSeconds ? ` (${fmtElapsed(b.elapsedSeconds)})` : ''}
                    {': '}{b.message}
                  </li>
                ))}
              </ul>
            </Alert>
          )}

          {/* ---- Preflight diagnostics banner ---- */}
          {snapshot.targetVersion && (() => {
            const tv = snapshot.targetVersion;
            const ch = snapshot.targetChannel ?? '';

            // No matching check for this target version
            if (!matchingCheck) {
              const isComplete = snapshot.phase === 'complete';
              return (
                <div style={{
                  marginBottom: 20, padding: '10px 16px',
                  background: T.infoBg,
                  borderLeft: `4px solid ${T.info}`,
                  borderRadius: T.radiusSm,
                  display: 'flex', alignItems: 'center', gap: 12,
                }}>
                  <div style={{ flex: 1, fontSize: 13, color: T.textSubtle }}>
                    <strong style={{ color: T.info }}>No preflight check found for {tv}.</strong>
                    {isComplete
                      ? ' Run a diagnostics check for a retrospective analysis of this upgrade.'
                      : ' Run a diagnostics check to identify potential roadblocks before or during this upgrade.'
                    }
                  </div>
                  {onRunCheckRequested && (
                    <Button
                      variant={isComplete ? 'secondary' : 'primary'}
                      size="sm"
                      onClick={() => onRunCheckRequested(tv, ch)}
                      style={{ borderRadius: T.radiusMd, whiteSpace: 'nowrap' }}
                    >
                      Run Check for {tv}
                    </Button>
                  )}
                </div>
              );
            }

            // Matching check found — determine severity
            const hasErrors = preflightIssues.some(c => c.result === 'Failed' || c.result === 'Error');
            const hasIssues = preflightIssues.length > 0;
            const bgColor   = hasErrors ? T.dangerBg  : hasIssues ? T.warningBg  : T.infoBg;
            const bdColor   = hasErrors ? T.danger     : hasIssues ? T.warning    : T.success;
            const hdColor   = hasErrors ? T.danger     : hasIssues ? T.warning    : T.success;
            const issueCount = preflightIssues.length;
            const heading   = hasErrors
              ? `Preflight checks found ${issueCount} issue${issueCount !== 1 ? 's' : ''} that may impact upgrade to ${tv}`
              : hasIssues
              ? `Preflight checks found ${issueCount} warning${issueCount !== 1 ? 's' : ''} for upgrade to ${tv}`
              : `Preflight checks passed for ${tv}`;

            // Snapshot timestamps — always shown so users can judge data freshness.
            const checkStatus = matchingCheck.status;
            const ranAt       = checkStatus?.startedAt   ? new Date(checkStatus.startedAt)   : null;
            const completedAt = checkStatus?.completedAt ? new Date(checkStatus.completedAt) : null;
            const snapshotAge = completedAt ?? ranAt;
            const snapshotAgeMs = snapshotAge ? Date.now() - snapshotAge.getTime() : null;

            const fmtTs = (d: Date) => d.toLocaleString(undefined, {
              month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
            });

            return (
              <div style={{
                marginBottom: 20, padding: '10px 16px',
                background: bgColor,
                borderLeft: `4px solid ${bdColor}`,
                borderRadius: T.radiusSm,
              }}>
                <div style={{ fontWeight: 700, fontSize: 13, color: hdColor, marginBottom: 4 }}>
                  {heading}
                </div>

                {/* Context-aware snapshot relevance pill — always visible */}
                {ranAt && (() => {
                  const isPreUpgrade = checkContext === 'pre-upgrade-snapshot';
                  const isDuringUpgrade = checkContext === 'during-upgrade';
                  const isVeryStale = checkContext === 'very-stale';
                  const isStale = checkContext === 'stale';
                  const isAging = checkContext === 'aging';

                  const pillColor = isPreUpgrade ? T.success
                    : isDuringUpgrade ? T.info
                    : isVeryStale ? T.danger
                    : (isStale || isAging) ? T.warning
                    : T.textMuted;

                  const pillLabel = isPreUpgrade
                    ? '✓ Pre-upgrade snapshot'
                    : isDuringUpgrade
                    ? 'Taken during upgrade'
                    : isVeryStale
                    ? '⚠ Very stale snapshot'
                    : isStale
                    ? '⚠ Stale snapshot'
                    : isAging
                    ? '⚠ Aging snapshot'
                    : 'Recent snapshot';

                  const showRerunLink = (isAging || isStale || isVeryStale) && onRunCheckRequested;

                  return (
                    <div style={{
                      display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 6,
                      marginBottom: 8,
                    }}>
                      <div style={{
                        display: 'inline-flex', alignItems: 'center', gap: 6,
                        fontSize: 11, fontWeight: isPreUpgrade ? 600 : 400,
                        color: pillColor,
                        background: `${pillColor}18`,
                        border: `1px solid ${pillColor}55`,
                        borderRadius: T.radiusPill,
                        padding: '2px 8px',
                      }}>
                        <span>{pillLabel}</span>
                        <span style={{ color: T.textSubtle }}>·</span>
                        <span>Run: {fmtTs(ranAt)}</span>
                        {completedAt && completedAt.getTime() !== ranAt.getTime() && (
                          <span style={{ color: T.textSubtle }}>· Completed: {fmtTs(completedAt)}</span>
                        )}
                        {snapshotAgeMs !== null && (
                          <span>· {fmtAge(snapshotAgeMs)} ago</span>
                        )}
                      </div>
                      {showRerunLink && (
                        <button
                          onClick={() => onRunCheckRequested!(tv, ch)}
                          style={{ background: 'none', border: `1px solid ${T.info}55`, cursor: 'pointer',
                            padding: '2px 8px', borderRadius: T.radiusPill,
                            color: T.info, font: 'inherit', fontSize: 11 }}
                        >
                          Re-run check
                        </button>
                      )}
                      {isVeryStale && (
                        <span style={{ fontSize: 11, color: T.danger, fontStyle: 'italic' }}>
                          Cluster state may have changed significantly — results may not be relevant.
                        </span>
                      )}
                    </div>
                  );
                })()}

                <div style={{ fontSize: 12, color: T.textSubtle }}>
                  {hasIssues
                    ? <>{preflightIssues.map(c => c.name.replace(/([a-z])([A-Z])/g, '$1 $2')).join(', ')}
                        {' — '}
                        {'See the '}
                        <strong>Diagnostics Checks</strong>
                        {' tab for details and remediation steps.'}</>
                    : <>All checks passed. See the <strong>Diagnostics Checks</strong> tab for the full report.</>
                  }
                </div>
              </div>
            );
          })()}

          {/* ---- Cluster Operators (collapsed by default) ---- */}
          <OperatorsCard ops={snapshot.clusterOperators} />

          {/* ---- Machine Pools ---- */}
          {snapshot.machinePools && snapshot.machinePools.length > 0 && (
            <div style={cardStyle}>
              <SectionHeader title="Machine Config Pools" />
              <div style={{ padding: '14px 16px' }}>
                {snapshot.machinePools.map(pool => (
                  <MachinePoolCard key={pool.name} pool={pool} />
                ))}
              </div>
            </div>
          )}

          {/* ---- Machine Health Checks ---- */}
          {snapshot.machineHealthChecks && snapshot.machineHealthChecks.length > 0 && (
            <MachineHealthChecksCard mhcs={snapshot.machineHealthChecks} />
          )}
        </>
      )}

      {/* ---- Upgrade History ---- */}
      {history && history.history && history.history.length > 0 && (
        <div style={cardStyle}>
          <SectionHeader
            title="Upgrade History"
            badge={
              history.currentChannel
                ? <span style={{
                    fontSize: 11, fontWeight: 600, background: T.infoBg,
                    border: `1px solid ${T.info}55`, borderRadius: T.radiusPill,
                    padding: '2px 8px', color: T.info,
                  }}>
                    {history.currentChannel}
                  </span>
                : undefined
            }
          />
          <div style={{ overflowX: 'auto' }}>
            <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
              <thead>
                <tr style={{ background: T.headerBg }}>
                  {(['Version', 'State', 'Started', 'Completed', 'Duration'] as const).map(h => (
                    <th key={h} style={{
                      padding: '8px 16px', textAlign: 'left',
                      fontSize: 11, fontWeight: 600, color: T.textSubtle,
                      textTransform: 'uppercase', letterSpacing: '0.05em',
                      borderBottom: `1px solid ${T.border}`,
                    }}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {history.history.map((entry, i) => {
                  const isCompleted = entry.state === 'Completed';
                  const isPartial   = entry.state === 'Partial';
                  const stateColor  = isCompleted ? T.success : isPartial ? T.warning : T.textMuted;
                  const stateBg     = isCompleted ? T.successBg : isPartial ? T.warningBg : 'transparent';

                  const fmtDate = (iso?: string) => {
                    if (!iso) return '—';
                    const d = new Date(iso);
                    return d.toLocaleString(undefined, {
                      year: 'numeric', month: 'short', day: 'numeric',
                      hour: '2-digit', minute: '2-digit',
                    });
                  };

                  return (
                    <tr key={i} style={{ borderBottom: `1px solid ${T.border}` }}>
                      {/* Version: fromVersion → version */}
                      <td style={{ padding: '10px 16px', fontFamily: 'monospace', fontWeight: 600 }}>
                        {entry.fromVersion
                          ? <>{entry.fromVersion} <span style={{ color: T.textSubtle }}>→</span> {entry.version}</>
                          : entry.version
                        }
                      </td>
                      {/* State badge */}
                      <td style={{ padding: '10px 16px' }}>
                        <span style={{
                          display: 'inline-block', fontSize: 11, fontWeight: 600,
                          padding: '2px 8px', borderRadius: T.radiusPill,
                          background: stateBg, color: stateColor,
                          border: `1px solid ${stateColor}55`,
                        }}>
                          {entry.state}
                        </span>
                      </td>
                      {/* Started */}
                      <td style={{ padding: '10px 16px', color: T.textSubtle, whiteSpace: 'nowrap' }}>
                        {fmtDate(entry.startedAt)}
                      </td>
                      {/* Completed */}
                      <td style={{ padding: '10px 16px', color: T.textSubtle, whiteSpace: 'nowrap' }}>
                        {fmtDate(entry.completedAt)}
                      </td>
                      {/* Duration */}
                      <td style={{ padding: '10px 16px', fontWeight: entry.elapsedSeconds ? 600 : 400 }}>
                        {entry.elapsedSeconds ? fmtElapsed(entry.elapsedSeconds) : '—'}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
};

export default UpgradeStatusView;
