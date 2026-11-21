import * as React from 'react';
import {
  Page, PageSection, PageSectionVariants, Title,
  Button, Spinner, Alert, EmptyState, EmptyStateBody,
} from '@patternfly/react-core';
import { TrashIcon, SyncAltIcon, PlusCircleIcon, CaretDownIcon, CaretRightIcon, InfoCircleIcon } from '@patternfly/react-icons';
import { useK8sWatchResource, consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { type UpgradeDiagsCheck } from '../types';
import { StatusBadge } from './StatusBadge';
import { UpgradeDiagsDetail } from './UpgradeDiagsDetail';
import { CreateDiagsCheckForm, type InitialValues } from './CreateDiagsCheckForm';
import { UpgradeStatusView } from './UpgradeStatusView';

const MUO_NS = 'openshift-managed-upgrade-operator';
const UPGRADEDIAGS_REQUEST_ID_LABEL = 'upgradediags.managed.openshift.io/request-id';
const UPGRADEDIAGS_DESCRIPTION_ANNOTATION = 'upgradediags.managed.openshift.io/description';
const UPGRADEDIAGS_PROXY_BASE = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/upgrade-diags';

const T = {
  danger:      'var(--pf-t--global--color--status--danger--default)',
  warning:     'var(--pf-t--global--color--status--warning--default)',
  success:     'var(--pf-t--global--color--status--success--default)',
  info:        'var(--pf-t--global--color--status--info--default)',
  textSubtle:  'var(--pf-t--global--color--text--subtle)',
  textMuted:   'var(--pf-t--global--color--text--placeholder)',
  border:      'var(--pf-t--global--border--color--default)',
  bgSecondary: 'var(--pf-t--global--background--color--secondary--default)',
  bgHover:     'var(--pf-t--global--background--color--secondary--hover)',
  bgActive:    'var(--pf-t--global--background--color--secondary--click)',
  infoBg:      'var(--pf-t--global--background--color--status--info--default)',
  shadow:      'var(--pf-t--global--shadow--md)',
  radiusPill: 'var(--pf-t--global--border--radius--pill)',
  radiusLg:   'var(--pf-t--global--border--radius--lg)',
  radiusMd:   'var(--pf-t--global--border--radius--md)',
  radiusSm:   'var(--pf-t--global--border--radius--sm)',
} as const;

const relativeTime = (ts?: string): string => {
  if (!ts) return '—';
  const diff = Date.now() - new Date(ts).getTime();
  if (diff < 60_000) return 'just now';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} min ago`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} hr ago`;
  return new Date(ts).toLocaleDateString();
};

const resultBorder = (r?: string) => {
  if (r === 'Failed')  return T.danger;
  if (r === 'Error')   return T.warning;
  if (r === 'Warning') return T.warning;
  if (r === 'Passed')  return T.success;
  return 'transparent';
};

// Inline-styled colored summary counts — no PF CSS class dependency
const CheckSummary: React.FC<{ pfc: UpgradeDiagsCheck }> = ({ pfc }) => {
  const s = pfc.status;
  if (!s || (!s.completedAt && !s.startedAt)) {
    return <Spinner size="sm" aria-label="Waiting to start" />;
  }
  if (s.startedAt && !s.completedAt) {
    const done = (s.checksPassed ?? 0) + (s.checksFailed ?? 0) + (s.checksErrored ?? 0) + (s.checksWarned ?? 0);
    const total = s.checks?.length ?? 0;
    return <span style={{ color: T.info }}>Running{total > 0 ? ` (${done}/${total})` : '…'}</span>;
  }

  const pills: Array<[number | undefined, string, string]> = [
    [s.checksPassed,  'passed',  T.success],
    [s.checksFailed,  'failed',  T.danger],
    [s.checksErrored, (s.checksErrored ?? 0) === 1 ? 'error' : 'errors', T.warning],
    [s.checksWarned,  (s.checksWarned  ?? 0) === 1 ? 'warning' : 'warnings', T.warning],
  ];

  return (
    <span style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
      {pills.filter(([n]) => (n ?? 0) > 0).map(([n, label, color]) => (
        <span key={label} style={{
          display: 'inline-flex', alignItems: 'center', gap: 4,
          padding: '1px 8px', borderRadius: T.radiusPill,
          background: T.bgSecondary, color, border: `1px solid ${T.border}`,
          fontWeight: 600, fontSize: 12,
        }}>
          {n} {label}
        </span>
      ))}
    </span>
  );
};

type Tab = 'checks' | 'status';

const TAB_LABELS: Record<Tab, string> = {
  checks: 'Reports',
  status: 'Upgrade Status',
};

// Tab description bar: summary always visible; full detail behind an info toggle.
const TabDescription: React.FC<{ summary: string; detail: string }> = ({ summary, detail }) => {
  const [open, setOpen] = React.useState(false);
  return (
    <div style={{
      padding: '10px 20px',
      background: T.bgSecondary,
      borderBottom: `1px solid ${T.border}`,
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 13, color: T.textSubtle }}>{summary}</span>
        <button
          onClick={() => setOpen(o => !o)}
          aria-label={open ? 'Hide details' : 'Show details'}
          style={{
            background: 'none', border: 'none', cursor: 'pointer', padding: '0 2px',
            color: open ? T.info : T.textMuted,
            display: 'inline-flex', alignItems: 'center', lineHeight: 1,
          }}
        >
          <InfoCircleIcon />
        </button>
      </div>
      {open && (
        <div style={{
          marginTop: 8, padding: '8px 12px',
          background: T.infoBg,
          borderLeft: `3px solid ${T.info}`,
          borderRadius: T.radiusSm,
          fontSize: 13, color: T.textSubtle, lineHeight: 1.6, maxWidth: 860,
          whiteSpace: 'pre-wrap',
        }}>
          {detail}
        </div>
      )}
    </div>
  );
};

export const UpgradeDiagsPage: React.FC = () => {
  const [activeTab, setActiveTab]         = React.useState<Tab>('checks');
  const [createOpen, setCreateOpen]       = React.useState(false);
  const [createInitial, setCreateInitial] = React.useState<InitialValues | undefined>();
  const [expanded, setExpanded]           = React.useState<Record<string, boolean>>({});
  const [hoveredRow, setHoveredRow]       = React.useState<string | null>(null);
  const [deleting, setDeleting]           = React.useState<Record<string, boolean>>({});
  const [deleteError, setDeleteError]     = React.useState<string | null>(null);

  const [checks, loaded, loadError] = useK8sWatchResource<UpgradeDiagsCheck[]>({
    groupVersionKind: { group: 'upgrade.managed.openshift.io', version: 'v1alpha1', kind: 'UpgradeDiagsCheck' },
    namespace: MUO_NS,
    isList: true,
  });

  const sorted = React.useMemo(
    () => [...(checks ?? [])].sort((a, b) =>
      (b.metadata?.creationTimestamp ?? '').localeCompare(a.metadata?.creationTimestamp ?? '')
    ),
    [checks],
  );

  const toggle = (name: string) =>
    setExpanded(prev => ({ ...prev, [name]: !prev[name] }));

  const openCreate = (initial?: InitialValues) => {
    setCreateInitial(initial);
    setCreateOpen(true);
  };

  const handleDelete = async (pfc: UpgradeDiagsCheck) => {
    const name = pfc.metadata?.name ?? '';
    const requestId = pfc.metadata?.labels?.[UPGRADEDIAGS_REQUEST_ID_LABEL];
    if (!requestId) {
      setDeleteError(`"${name}" was not created via the HTTP API and cannot be deleted here.`);
      return;
    }
    setDeleting(prev => ({ ...prev, [name]: true }));
    setDeleteError(null);
    try {
      await consoleFetch(`${UPGRADEDIAGS_PROXY_BASE}/${requestId}`, { method: 'DELETE' });
    } catch (e: unknown) {
      setDeleteError(e instanceof Error ? e.message : 'Delete failed');
      setDeleting(prev => ({ ...prev, [name]: false }));
    }
  };

  const handleRerun = (pfc: UpgradeDiagsCheck) => {
    openCreate({
      version:     pfc.spec.desired.version,
      channel:     pfc.spec.desired.channel,
      platform:    pfc.spec.type,
      description: pfc.metadata?.annotations?.[UPGRADEDIAGS_DESCRIPTION_ANNOTATION] ?? '',
    });
  };

  const cardStyle: React.CSSProperties = {
    border: `1px solid ${T.border}`,
    borderRadius: T.radiusLg,
    overflow: 'hidden',
    boxShadow: T.shadow,
  };

  const thStyle: React.CSSProperties = {
    padding: '10px 16px',
    textAlign: 'left',
    fontWeight: 600,
    fontSize: 12,
    textTransform: 'uppercase',
    letterSpacing: '0.05em',
    color: T.textSubtle,
    borderBottom: `1px solid ${T.border}`,
    background: T.bgSecondary,
    whiteSpace: 'nowrap',
  };

  return (
    <Page>
      <PageSection variant={PageSectionVariants.light}>
        <Title headingLevel="h1">Upgrade Diagnostics</Title>
        {/* Tab bar */}
        <div style={{ display: 'flex', gap: 0, marginTop: 16, borderBottom: `1px solid ${T.border}` }}>
          {(Object.keys(TAB_LABELS) as Tab[]).map(tab => {
            const active = activeTab === tab;
            return (
              <button
                key={tab}
                onClick={() => setActiveTab(tab)}
                style={{
                  background: 'none',
                  border: 'none',
                  borderBottom: active ? `3px solid ${T.info}` : '3px solid transparent',
                  color: active ? T.info : T.textSubtle,
                  cursor: 'pointer',
                  fontWeight: active ? 700 : 400,
                  fontSize: 14,
                  padding: '8px 20px 10px',
                  transition: 'all 0.15s',
                }}
              >
                {TAB_LABELS[tab]}
              </button>
            );
          })}
        </div>
      </PageSection>

      <TabDescription
        summary={
          activeTab === 'checks'
            ? 'Reports — Read-only diagnostic reports against a target upgrade version. Run at any time: before, during, or after an upgrade.'
            : 'Upgrade Status — Live snapshot of an in-progress or recently completed cluster upgrade. Auto-refreshes every 30s.'
        }
        detail={
          activeTab === 'checks'
            ? 'Each diagnostic report is entirely read-only — no upgrade is triggered and no cluster state is modified. Reports run a suite of checks against the current cluster state to surface issues across multiple categories: admin acknowledgements, critical alerts, PodDisruptionBudgets that could stall node drains, pull secret validity, operator health, and more.\n\nUseful at every stage of an upgrade lifecycle: assess readiness before scheduling a maintenance window, diagnose why an in-progress upgrade is delayed or stuck, understand what went wrong after a failed upgrade, or validate cluster health at any point in time.'
            : 'Tracks ClusterVersion, ClusterOperators, MachineConfigPools, nodes, and MachineHealthChecks. Click any operator or node row to drill into conditions, PDB drain blockers, cloud-provider errors, and K8s node conditions — with actionable remediation guidance inline.'
        }
      />

      <PageSection>
        {activeTab === 'status' && (
          <UpgradeStatusView
            onRunCheckRequested={(version, channel) => {
              openCreate({ version, channel, platform: 'OSD', description: '' });
              setActiveTab('checks');
            }}
          />
        )}
        {activeTab === 'checks' && (<>
        {deleteError && (
          <Alert variant="warning" isInline title="Delete failed" style={{ marginBottom: 16 }}>
            {deleteError}
          </Alert>
        )}
        {loadError && (
          <Alert variant="warning" isInline title="Failed to load checks">
            {String(loadError)}
          </Alert>
        )}
        {!loaded && <Spinner />}

        {loaded && !loadError && sorted.length === 0 && !createOpen && (
          <EmptyState>
            <EmptyStateBody>
              No diagnostic reports have been generated yet. Run diagnostics at any time to assess cluster upgrade readiness, diagnose an in-progress or delayed upgrade, or analyse a failed upgrade.
            </EmptyStateBody>
            <Button variant="primary" onClick={() => openCreate()} style={{ marginTop: 16, borderRadius: T.radiusMd }}>
              Run Diagnostics
            </Button>
          </EmptyState>
        )}

        {loaded && sorted.length > 0 && (
          <div style={cardStyle}>
            <table style={{ width: '100%', borderCollapse: 'collapse', tableLayout: 'fixed' }}>
              <colgroup>
                <col style={{ width: 36 }} />
                <col style={{ minWidth: 180 }} />
                <col style={{ width: 120 }} />
                <col />
                <col style={{ width: 130 }} />
                <col style={{ width: 90 }} />
              </colgroup>
              <thead>
                <tr>
                  <th style={thStyle} />
                  <th style={thStyle}>Target</th>
                  <th style={thStyle}>Result</th>
                  <th style={thStyle}>Checks</th>
                  <th style={thStyle}>Started</th>
                  <th style={thStyle} />
                </tr>
              </thead>
              <tbody>
                {sorted.map((pfc) => {
                  const name        = pfc.metadata?.name ?? '';
                  const isExpanded  = !!expanded[name];
                  const isDeleting  = !!deleting[name];
                  const isHovered   = hoveredRow === name;
                  const description = pfc.metadata?.annotations?.[UPGRADEDIAGS_DESCRIPTION_ANNOTATION];
                  const border      = resultBorder(pfc.status?.overallResult);

                  const rowBg = isExpanded
                    ? T.bgActive
                    : isHovered
                    ? T.bgSecondary
                    : 'transparent';

                  const tdStyle: React.CSSProperties = {
                    padding: '12px 16px',
                    borderBottom: `1px solid ${T.border}`,
                    verticalAlign: 'middle',
                    background: rowBg,
                    transition: 'background 0.1s',
                  };

                  return (
                    <React.Fragment key={name}>
                      <tr
                        style={{ cursor: 'pointer', borderLeft: `4px solid ${border}` }}
                        onClick={() => toggle(name)}
                        onMouseEnter={() => setHoveredRow(name)}
                        onMouseLeave={() => setHoveredRow(null)}
                      >
                        <td style={{ ...tdStyle, padding: '12px 8px', textAlign: 'center', color: T.textMuted }}>
                          {isExpanded ? <CaretDownIcon /> : <CaretRightIcon />}
                        </td>
                        <td style={tdStyle}>
                          <div style={{ fontWeight: 600, fontSize: 15 }}>{pfc.spec.desired.version}</div>
                          <div style={{ fontSize: 12, color: T.textSubtle, marginTop: 2 }}>
                            {pfc.spec.desired.channel}
                          </div>
                          {description && (
                            <div style={{ fontSize: 12, color: T.textMuted, marginTop: 2, fontStyle: 'italic' }}>
                              {description}
                            </div>
                          )}
                        </td>
                        <td style={tdStyle}>
                          {pfc.status?.overallResult
                            ? <StatusBadge result={pfc.status.overallResult} />
                            : <Spinner size="sm" />}
                        </td>
                        <td style={tdStyle}><CheckSummary pfc={pfc} /></td>
                        <td style={{ ...tdStyle, color: T.textMuted, fontSize: 13 }}>
                          {relativeTime(pfc.metadata?.creationTimestamp)}
                        </td>
                        <td
                          style={{ ...tdStyle, textAlign: 'right', whiteSpace: 'nowrap' }}
                          onClick={e => e.stopPropagation()}
                        >
                          <Button
                            variant="plain"
                            aria-label="Re-run with same settings"
                            title="Re-run with same settings"
                            onClick={() => handleRerun(pfc)}
                            isDisabled={isDeleting}
                            style={{ color: T.textSubtle }}
                          >
                            <SyncAltIcon />
                          </Button>
                          <Button
                            variant="plain"
                            aria-label="Delete this check run"
                            title="Delete"
                            onClick={() => handleDelete(pfc)}
                            isDisabled={isDeleting}
                            style={{ color: isDeleting ? T.textMuted : T.danger }}
                          >
                            {isDeleting ? <Spinner size="sm" /> : <TrashIcon />}
                          </Button>
                        </td>
                      </tr>

                      {isExpanded && (
                        <tr>
                          <td
                            colSpan={6}
                            style={{
                              borderLeft: `4px solid ${border}`,
                              borderBottom: `1px solid ${T.border}`,
                              padding: 0,
                              background: T.bgSecondary,
                            }}
                          >
                            <UpgradeDiagsDetail pfc={pfc} />
                          </td>
                        </tr>
                      )}
                    </React.Fragment>
                  );
                })}

                {/* Run Diagnostics row */}
                <tr>
                  <td
                    colSpan={6}
                    style={{
                      padding: '12px 16px',
                      textAlign: 'center',
                      borderTop: sorted.length > 0 ? `1px solid ${T.border}` : undefined,
                    }}
                  >
                    {!createOpen ? (
                      <Button
                        variant="link"
                        icon={<PlusCircleIcon />}
                        onClick={() => openCreate()}
                      >
                        Run Diagnostics
                      </Button>
                    ) : (
                      <span style={{ color: T.textMuted, fontSize: 13 }}>
                        Fill in the form below to run diagnostics
                      </span>
                    )}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        )}

        {/* Inline form panel — no Modal, stays within iframe bounds */}
        <CreateDiagsCheckForm
          isOpen={createOpen}
          onClose={() => setCreateOpen(false)}
          onCreated={() => setCreateOpen(false)}
          initialValues={createInitial}
        />
        </>)}
      </PageSection>
    </Page>
  );
};

export default UpgradeDiagsPage;
