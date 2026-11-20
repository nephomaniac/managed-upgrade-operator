import * as React from 'react';
import {
  CheckCircleIcon, ExclamationTriangleIcon, TimesCircleIcon,
  ExclamationCircleIcon, BanIcon, ClockIcon, QuestionCircleIcon,
} from '@patternfly/react-icons';
import { type CheckResult } from '../types';

// Inline-styled badge — does not depend on PF CSS class loading.
// PF's pf-m-red / pf-m-green etc. are not guaranteed to load in
// the console plugin's iframe context.

const C = {
  success: 'var(--pf-t--global--color--status--success--default)',
  danger:  'var(--pf-t--global--color--status--danger--default)',
  warning: 'var(--pf-t--global--color--status--warning--default)',
  info:    'var(--pf-t--global--color--status--info--default)',
  subtle:  'var(--pf-t--global--color--text--subtle)',
  successBg: 'var(--pf-t--global--background--color--status--success--default)',
  dangerBg:  'var(--pf-t--global--background--color--status--danger--default)',
  warningBg: 'var(--pf-t--global--background--color--status--warning--default)',
  infoBg:    'var(--pf-t--global--background--color--status--info--default)',
  subtleBg:  'var(--pf-t--global--background--color--secondary--default)',
  border:    'var(--pf-t--global--border--color--default)',
  radiusPill: 'var(--pf-t--global--border--radius--pill)',
  radiusLg:   'var(--pf-t--global--border--radius--lg)',
  radiusMd:   'var(--pf-t--global--border--radius--md)',
  radiusSm:   'var(--pf-t--global--border--radius--sm)',
} as const;

interface Config {
  label: string;
  bg: string;
  fg: string;
  border: string;
  icon: React.ReactNode;
}

const CONFIG: Record<CheckResult, Config> = {
  Passed:   { label: 'Passed',    bg: C.successBg, fg: C.success, border: C.border, icon: <CheckCircleIcon /> },
  Failed:   { label: 'Failed',    bg: C.dangerBg,  fg: C.danger,  border: C.border, icon: <TimesCircleIcon /> },
  Error:    { label: 'Error',     bg: C.dangerBg,  fg: C.danger,  border: C.border, icon: <ExclamationCircleIcon /> },
  Warning:  { label: 'Warning',   bg: C.warningBg, fg: C.warning, border: C.border, icon: <ExclamationTriangleIcon /> },
  Skipped:  { label: 'Skipped',   bg: C.subtleBg,  fg: C.subtle,  border: C.border, icon: <BanIcon /> },
  TimedOut: { label: 'Timed Out', bg: C.dangerBg,  fg: C.danger,  border: C.border, icon: <ClockIcon /> },
  Unknown:  { label: 'Unknown',   bg: C.subtleBg,  fg: C.subtle,  border: C.border, icon: <QuestionCircleIcon /> },
};

interface Props { result: CheckResult; }

export const StatusBadge: React.FC<Props> = ({ result }) => {
  const { label, bg, fg, border, icon } = CONFIG[result] ?? CONFIG.Unknown;
  return (
    <span style={{
      display: 'inline-flex',
      alignItems: 'center',
      gap: 5,
      padding: '3px 10px',
      borderRadius: C.radiusPill,
      background: bg,
      color: fg,
      border: `1px solid ${border}`,
      fontWeight: 600,
      fontSize: 13,
      whiteSpace: 'nowrap',
    }}>
      {icon}
      {label}
    </span>
  );
};
