import * as React from 'react';
import { List, ListItem } from '@patternfly/react-core';
import { InfoCircleIcon } from '@patternfly/react-icons';
import { type UpgradeDiagsCheck, type DiagsCheckResult, type CheckResult, type StructuredFinding } from '../types';
import { StatusBadge } from './StatusBadge';

interface Props { pfc: UpgradeDiagsCheck; }

const T = {
  danger:      'var(--pf-t--global--color--status--danger--default)',
  warning:     'var(--pf-t--global--color--status--warning--default)',
  success:     'var(--pf-t--global--color--status--success--default)',
  info:        'var(--pf-t--global--color--status--info--default)',
  textSubtle:  'var(--pf-t--global--color--text--subtle)',
  textMuted:   'var(--pf-t--global--color--text--placeholder)',
  border:      'var(--pf-t--global--border--color--default)',
  bgSecondary: 'var(--pf-t--global--background--color--secondary--default)',
  dangerBg:    'var(--pf-t--global--background--color--status--danger--default)',
  warningBg:   'var(--pf-t--global--background--color--status--warning--default)',
  infoBg:      'var(--pf-t--global--background--color--status--info--default)',
  radiusPill: 'var(--pf-t--global--border--radius--pill)',
  radiusLg:   'var(--pf-t--global--border--radius--lg)',
  radiusMd:   'var(--pf-t--global--border--radius--md)',
  radiusSm:   'var(--pf-t--global--border--radius--sm)',
} as const;

const UPGRADEDIAGS_DESCRIPTION_ANNOTATION = 'upgradediags.managed.openshift.io/description';

const SEVERITY: Record<CheckResult, number> = {
  Failed: 0, Error: 1, Warning: 2, Unknown: 3, Passed: 4, Skipped: 5, TimedOut: 6,
};

const humanize = (s: string) => s.replace(/([a-z])([A-Z])/g, '$1 $2');
const fmt      = (ts?: string) => ts ? new Date(ts).toLocaleString() : '—';

const elapsed = (start?: string, end?: string): string => {
  if (!start) return '—';
  const ms = (end ? new Date(end) : new Date()).getTime() - new Date(start).getTime();
  if (ms < 0) return '—';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
};

const truncate = (s: string, max = 100): string =>
  s.length > max ? s.slice(0, max) + '…' : s;

const checkBorderColor = (result: CheckResult): string => {
  if (result === 'Failed')  return T.danger;
  if (result === 'Error')   return T.danger;
  if (result === 'Warning') return T.warning;
  if (result === 'Passed')  return T.success;
  return 'transparent';
};

// Custom inline banner — avoids PF Alert whose variant text shows as literal
// "Danger alert:" in the console plugin iframe when PF CSS is not fully loaded.
const SummaryBanner: React.FC<{
  issueCount: number;
  customerItems: DiagsCheckResult[];
  redHatItems: DiagsCheckResult[];
  otherItems: DiagsCheckResult[];
  hasErrors: boolean;
}> = ({ issueCount, customerItems, redHatItems, otherItems, hasErrors }) => {
  const borderColor = hasErrors ? T.danger : T.warning;
  const bg          = hasErrors ? T.dangerBg : T.warningBg;
  const icon        = hasErrors ? '✕' : '⚠';

  return (
    <div style={{
      padding: '12px 16px',
      marginBottom: 16,
      borderLeft: `4px solid ${borderColor}`,
      background: bg,
      borderRadius: T.radiusSm,
    }}>
      <div style={{ fontWeight: 700, color: borderColor, marginBottom: 8 }}>
        {icon} {issueCount} issue{issueCount !== 1 ? 's' : ''} found — upgrade may be blocked
      </div>
      {customerItems.length > 0 && (
        <div style={{ fontSize: 13, marginBottom: 4 }}>
          <strong>Admin action required:</strong>{' '}
          {customerItems.map(c => humanize(c.name)).join(', ')}
        </div>
      )}
      {redHatItems.length > 0 && (
        <div style={{ fontSize: 13, marginBottom: 4 }}>
          <strong>Red Hat action:</strong>{' '}
          {redHatItems.map(c => humanize(c.name)).join(', ')}
        </div>
      )}
      {otherItems.length > 0 && (
        <div style={{ fontSize: 13 }}>
          <strong>Needs attention:</strong>{' '}
          {otherItems.map(c => humanize(c.name)).join(', ')}
        </div>
      )}
    </div>
  );
};

// Severity color mapping for structured findings.
const sevColor = (sev?: StructuredFinding['severity']): string => {
  if (sev === 'critical' || sev === 'high') return T.danger;
  if (sev === 'medium') return T.warning;
  if (sev === 'low') return T.info;
  return T.textSubtle;
};

// Rich card list for structured findings — groups per-finding severity, owner, resource, and action.
const StructuredFindingsList: React.FC<{ findings: StructuredFinding[] }> = ({ findings }) => (
  <div style={{ marginBottom: 8 }}>
    <div style={{ fontWeight: 600, marginBottom: 6, fontSize: 13 }}>
      Findings ({findings.length}):
    </div>
    <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
      {findings.map((sf, i) => {
        const color = sevColor(sf.severity);
        const ownerLabel = sf.ownerLabel
          ?? (sf.owner === 'customer' ? 'Admin action required'
            : sf.owner === 'service-team' ? 'Red Hat action required'
            : null);
        return (
          <div key={i} style={{
            padding: '8px 12px',
            borderLeft: `3px solid ${color}`,
            background: `${color}08`,
            borderRadius: T.radiusSm,
          }}>
            {/* Badges row */}
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', alignItems: 'center', marginBottom: 4 }}>
              {sf.severity && (
                <span style={{
                  display: 'inline-flex', padding: '1px 7px', borderRadius: T.radiusPill,
                  fontSize: 10, fontWeight: 700, textTransform: 'uppercase' as const,
                  background: `${color}22`, color, border: `1px solid ${color}55`,
                }}>
                  {sf.severity}
                </span>
              )}
              {ownerLabel && (
                <span style={{
                  display: 'inline-flex', padding: '1px 7px', borderRadius: T.radiusPill,
                  fontSize: 10, fontWeight: 600,
                  background: sf.owner === 'customer' ? T.warningBg : T.infoBg,
                  color: sf.owner === 'customer' ? T.warning : T.info,
                  border: `1px solid ${T.border}`,
                }}>
                  {ownerLabel}
                </span>
              )}
              {sf.resource && (
                <span style={{
                  display: 'inline-flex', alignItems: 'center', padding: '1px 7px',
                  borderRadius: T.radiusPill, fontSize: 10, fontFamily: 'monospace',
                  background: T.bgSecondary, color: T.textSubtle, border: `1px solid ${T.border}`,
                }}>
                  {sf.resource.kind}
                  {sf.resource.namespace ? `/${sf.resource.namespace}` : ''}
                  /{sf.resource.name}
                </span>
              )}
            </div>
            {/* Message */}
            <div style={{ fontSize: 13 }}>{sf.message}</div>
            {/* Per-finding action */}
            {sf.action && (
              <div style={{ marginTop: 4, fontSize: 12, color: T.textSubtle, fontStyle: 'italic' }}>
                ⚑ {sf.action}
              </div>
            )}
          </div>
        );
      })}
    </div>
  </div>
);

// Expanded check row content
const CheckRowDetail: React.FC<{ c: DiagsCheckResult }> = ({ c }) => {
  // Prefer backend-provided ownerLabel, fall back to client-side translation for backwards compatibility
  const ownerLabel = c.ownerLabel || (
    c.owner === 'customer'
      ? 'Admin action required'
      : c.owner === 'service-team'
      ? 'Red Hat action required'
      : null
  );

  const hasContent = c.execution.summary || c.action || c.execution.error ||
    (c.execution.findings && c.execution.findings.length > 0) ||
    (c.structuredFindings && c.structuredFindings.length > 0) || ownerLabel;

  if (!hasContent) {
    return <div style={{ padding: '8px 0', color: T.textMuted, fontSize: 13 }}>No additional details.</div>;
  }

  return (
    <div style={{ padding: '10px 0 10px 16px' }}>
      {/* Full description */}
      {c.description && (
        <div style={{
          marginBottom: 12, fontSize: 13, color: T.textSubtle,
          lineHeight: 1.6, fontStyle: 'italic',
        }}>
          {c.description}
        </div>
      )}

      {/* Owner badge */}
      {ownerLabel && (c.result === 'Failed' || c.result === 'Error' || c.result === 'Warning') && (
        <div style={{ marginBottom: 10 }}>
          <span style={{
            display: 'inline-flex', alignItems: 'center', gap: 4,
            padding: '3px 10px', borderRadius: T.radiusPill, fontWeight: 600, fontSize: 12,
            background: c.owner === 'customer' ? T.warningBg : T.infoBg,
            color: c.owner === 'customer' ? T.warning : T.info,
            border: `1px solid ${T.border}`,
          }}>
            {ownerLabel}
          </span>
        </div>
      )}

      {/* Summary text */}
      {c.execution.summary && (
        <div style={{ marginBottom: 8, fontSize: 14 }}>{c.execution.summary}</div>
      )}

      {/* Error */}
      {c.execution.error && (
        <div style={{
          marginBottom: 10, padding: '8px 12px',
          background: T.dangerBg,
          borderLeft: `3px solid ${T.danger}`,
          borderRadius: T.radiusSm, fontSize: 13, fontFamily: 'monospace',
          wordBreak: 'break-word',
        }}>
          {c.execution.error}
        </div>
      )}

      {/* Structured findings — rich per-finding cards when available */}
      {c.structuredFindings && c.structuredFindings.length > 0 && (
        <StructuredFindingsList findings={c.structuredFindings} />
      )}

      {/* Legacy string findings — only shown when no structured findings are present */}
      {(!c.structuredFindings || c.structuredFindings.length === 0) &&
        c.execution.findings && c.execution.findings.length > 0 && (
        <div style={{ marginBottom: 8 }}>
          <div style={{ fontWeight: 600, marginBottom: 4, fontSize: 13 }}>Findings:</div>
          <List style={{ fontSize: 13 }}>
            {c.execution.findings.map((f, i) => <ListItem key={i}>{f}</ListItem>)}
          </List>
        </div>
      )}

      {/* Action required */}
      {c.action && (
        <div style={{
          marginTop: 8, padding: '8px 12px',
          background: c.owner === 'customer' ? T.warningBg : T.infoBg,
          borderLeft: `3px solid ${c.owner === 'customer' ? T.warning : T.info}`,
          borderRadius: T.radiusSm, fontSize: 13,
        }}>
          <strong>Action required:</strong> {c.action}
        </div>
      )}

      {/* Elapsed time */}
      {c.execution.startedAt && (
        <div style={{ marginTop: 10, fontSize: 12, color: T.textMuted }}>
          Duration: {elapsed(c.execution.startedAt, c.execution.completedAt)}
        </div>
      )}
    </div>
  );
};

export const UpgradeDiagsDetail: React.FC<Props> = ({ pfc }) => {
  const [expanded, setExpanded] = React.useState<Record<string, boolean>>({});
  const toggle = (name: string) => setExpanded(prev => ({ ...prev, [name]: !prev[name] }));

  const description = pfc.metadata?.annotations?.[UPGRADEDIAGS_DESCRIPTION_ANNOTATION];
  const s           = pfc.status;
  const sorted      = [...(s?.checks ?? [])].sort(
    (a, b) => (SEVERITY[a.result] ?? 3) - (SEVERITY[b.result] ?? 3)
  );

  const platformLabel = pfc.spec.type === 'OSD'
    ? 'OpenShift Dedicated'
    : pfc.spec.type === 'ARO'
    ? 'Azure Red Hat OpenShift'
    : pfc.spec.type;

  const issueChecks    = sorted.filter(c => c.result === 'Failed' || c.result === 'Error' || c.result === 'Warning');
  const customerItems  = issueChecks.filter(c => c.owner === 'customer');
  const redHatItems    = issueChecks.filter(c => c.owner === 'service-team');
  const otherItems     = issueChecks.filter(c => !c.owner || (c.owner !== 'customer' && c.owner !== 'service-team'));
  const issueCount     = issueChecks.length;
  const hasErrors      = sorted.some(c => c.result === 'Error' || c.result === 'Failed');

  const labelStyle: React.CSSProperties = {
    fontSize: 11, fontWeight: 700, textTransform: 'uppercase',
    letterSpacing: '0.06em', color: T.textSubtle, marginBottom: 2,
  };

  const metaItems: Array<{ label: string; value: React.ReactNode }> = [
    { label: 'Target version', value: <strong style={{ fontSize: 15 }}>{pfc.spec.desired.version}</strong> },
    { label: 'Channel',        value: pfc.spec.desired.channel },
    { label: 'Platform',       value: platformLabel },
    { label: 'Overall result', value: s?.overallResult ? <StatusBadge result={s.overallResult} /> : '—' },
    { label: 'Started',        value: fmt(s?.startedAt) },
    { label: 'Completed',      value: fmt(s?.completedAt) },
    { label: 'Duration',       value: elapsed(s?.startedAt, s?.completedAt) },
    ...(description ? [{ label: 'Description', value: description }] : []),
  ];

  const thStyle: React.CSSProperties = {
    padding: '8px 12px',
    textAlign: 'left' as const,
    fontWeight: 700,
    fontSize: 11,
    textTransform: 'uppercase' as const,
    letterSpacing: '0.05em',
    color: T.textSubtle,
    borderBottom: `1px solid ${T.border}`,
    whiteSpace: 'nowrap' as const,
  };

  const tdBase: React.CSSProperties = {
    padding: '10px 12px',
    verticalAlign: 'top',
    borderBottom: `1px solid ${T.border}`,
    fontSize: 13,
  };

  return (
    <div style={{ padding: '16px 20px' }}>

      {/* Summary banner */}
      {issueCount > 0 && (
        <SummaryBanner
          issueCount={issueCount}
          customerItems={customerItems}
          redHatItems={redHatItems}
          otherItems={otherItems}
          hasErrors={hasErrors}
        />
      )}

      {/* Metadata two-column grid */}
      <div style={{
        display: 'grid',
        gridTemplateColumns: '1fr 1fr',
        gap: '14px 32px',
        marginBottom: 20,
        padding: '14px 16px',
        background: T.bgSecondary,
        border: `1px solid ${T.border}`,
        borderRadius: T.radiusMd,
      }}>
        {metaItems.map(({ label, value }) => (
          <div key={label}>
            <div style={labelStyle}>{label}</div>
            <div style={{ fontSize: 14 }}>{value}</div>
          </div>
        ))}
      </div>

      {/* Individual checks table */}
      {sorted.length > 0 ? (
        <div style={{ border: `1px solid ${T.border}`, borderRadius: T.radiusMd, overflow: 'hidden' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr style={{ background: T.bgSecondary }}>
                <th style={{ ...thStyle, width: 28, padding: '8px 6px' }} />
                <th style={thStyle}>Check</th>
                <th style={{ ...thStyle, width: 110 }}>Result</th>
                <th style={{ ...thStyle, width: 100 }}>Status</th>
                <th style={{ ...thStyle, width: 170 }}>Started</th>
              </tr>
            </thead>
            <tbody>
              {sorted.map((c) => {
                const isExpanded   = !!expanded[c.name];
                const borderColor  = checkBorderColor(c.result);
                const stateLabel   = { Completed: 'Completed', Error: 'Error', Skipped: 'Skipped', TimedOut: 'Timed Out' }[c.execution.state] ?? c.execution.state;
                const hasDetail    = c.execution.error || c.action || c.execution.summary ||
                  (c.execution.findings?.length ?? 0) > 0 || (c.structuredFindings?.length ?? 0) > 0;

                return (
                  <React.Fragment key={c.name}>
                    <tr
                      style={{ borderLeft: `3px solid ${borderColor}`, cursor: hasDetail ? 'pointer' : 'default' }}
                      onClick={() => hasDetail && toggle(c.name)}
                    >
                      <td style={{ ...tdBase, padding: '10px 6px', textAlign: 'center', color: T.textMuted, fontSize: 11 }}>
                        {hasDetail ? (isExpanded ? '▾' : '▸') : ''}
                      </td>
                      <td style={tdBase}>
                        <div style={{ fontWeight: 600 }}>{humanize(c.name)}</div>
                        {c.description && (
                          <div style={{ display: 'flex', alignItems: 'flex-start', gap: 4, marginTop: 2 }}>
                            <span style={{ fontSize: 12, color: T.textMuted }}>
                              {truncate(c.description)}
                            </span>
                            {c.description.length > 100 && (
                              <button
                                title={c.description}
                                aria-label="Full description"
                                style={{
                                  background: 'none', border: 'none', cursor: 'pointer',
                                  padding: 0, color: T.info, fontSize: 11,
                                  lineHeight: 1, flexShrink: 0, marginTop: 1,
                                }}
                              >
                                <InfoCircleIcon />
                              </button>
                            )}
                          </div>
                        )}
                      </td>
                      <td style={{ ...tdBase, whiteSpace: 'nowrap' }}>
                        <StatusBadge result={c.result} />
                      </td>
                      <td style={{ ...tdBase, color: T.textSubtle }}>{stateLabel}</td>
                      <td style={{ ...tdBase, color: T.textMuted, fontSize: 12 }}>
                        {fmt(c.execution.startedAt)}
                      </td>
                    </tr>
                    {isExpanded && (
                      <tr style={{ borderLeft: `3px solid ${borderColor}`, background: T.bgSecondary }}>
                        <td colSpan={5} style={{ padding: 0 }}>
                          <CheckRowDetail c={c} />
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <div style={{ color: T.textMuted, fontSize: 13 }}>Checks have not started yet.</div>
      )}
    </div>
  );
};
