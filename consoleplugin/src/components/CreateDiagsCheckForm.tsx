import * as React from 'react';
import {
  Button, Form, FormGroup, FormSelect, FormSelectOption,
  FormHelperText, HelperText, HelperTextItem, TextInput, Spinner,
} from '@patternfly/react-core';
import { TimesIcon, SyncIcon } from '@patternfly/react-icons';
import { consoleFetch, useK8sWatchResource, type K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

// MUO's HTTP service creates the CR using the operator's service account,
// bypassing the sre-regular-user-validation admission webhook.
const UPGRADEDIAGS_API = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/upgrade-diags';
const AVAILABLE_VERSIONS_API = '/api/proxy/plugin/managed-upgrade-operator/muo-api/api/v1/available-versions';
const MUO_NS = 'openshift-managed-upgrade-operator';
const VERSION_CACHE_TTL_MS = 60 * 60 * 1000; // 1 hour

const T = {
  danger:      'var(--pf-t--global--color--status--danger--default)',
  warning:     'var(--pf-t--global--color--status--warning--default)',
  textSubtle:  'var(--pf-t--global--color--text--subtle)',
  textMuted:   'var(--pf-t--global--color--text--placeholder)',
  border:      'var(--pf-t--global--border--color--default)',
  bgSecondary: 'var(--pf-t--global--background--color--secondary--default)',
  dangerBg:    'var(--pf-t--global--background--color--status--danger--default)',
  warningBg:   'var(--pf-t--global--background--color--status--warning--default)',
  shadow:      'var(--pf-t--global--shadow--md)',
  radiusLg:    'var(--pf-t--global--border--radius--lg)',
  radiusSm:    'var(--pf-t--global--border--radius--sm)',
} as const;

interface ClusterVersion extends K8sResourceCommon {
  spec?: { channel?: string };
  status?: {
    desired?: { version?: string };
    history?: Array<{ version: string; state: string }>;
    availableUpdates?: Array<{ version: string }>;
    conditionalUpdates?: Array<{ release: { version: string }; risks?: Array<{ name: string }> }>;
  };
}

interface UpgradeConfig extends K8sResourceCommon {
  spec?: { type?: string };
}

interface Infrastructure extends K8sResourceCommon {
  status?: { platformStatus?: { type?: string } };
}

export interface InitialValues {
  version: string;
  channel: string;
  platform: 'OSD' | 'ARO';
  description: string;
}

interface Props {
  isOpen: boolean;
  onClose: () => void;
  onCreated: () => void;
  initialValues?: InitialValues;
}

const SEMVER_RE = /^\d+\.\d+\.\d+$/;
const CHANNEL_RE = /^(stable|fast|candidate|eus)-\d+\.\d+$/;

interface AvailableVersionsData {
  channel: string;
  currentVersion: string;
  availableUpdates: Array<{ version: string }>;
  conditionalUpdates: Array<{ version: string; risks?: string[] }>;
  cachedAt: string;
  cacheExpiresAt: string;
}

export const CreateDiagsCheckForm: React.FC<Props> = ({ isOpen, onClose, onCreated, initialValues }) => {
  const [version, setVersion]               = React.useState('');
  const [channel, setChannel]               = React.useState('');
  const [platform, setPlatform]             = React.useState<'OSD' | 'ARO'>('OSD');
  const [description, setDescription]       = React.useState('');
  const [useCustomVersion, setUseCustomVersion] = React.useState(false);
  const [useCustomChannel, setUseCustomChannel] = React.useState(false);
  const [error, setError]                   = React.useState<string | null>(null);
  const [submitting, setSubmitting]         = React.useState(false);

  // Dynamic version fetching state
  const [dynamicVersions, setDynamicVersions] = React.useState<AvailableVersionsData | null>(null);
  const [fetchingVersions, setFetchingVersions] = React.useState(false);
  const [versionsFetchError, setVersionsFetchError] = React.useState<string | null>(null);

  const [cv, cvLoaded, cvError] = useK8sWatchResource<ClusterVersion>({
    groupVersionKind: { group: 'config.openshift.io', version: 'v1', kind: 'ClusterVersion' },
    name: 'version',
  });

  // Detect cluster platform from existing UpgradeConfig (authoritative) or Infrastructure (fallback).
  const [upgradeConfigs] = useK8sWatchResource<UpgradeConfig[]>({
    groupVersionKind: { group: 'upgrade.managed.openshift.io', version: 'v1alpha1', kind: 'UpgradeConfig' },
    namespace: MUO_NS,
    isList: true,
  });
  const [infra] = useK8sWatchResource<Infrastructure>({
    groupVersionKind: { group: 'config.openshift.io', version: 'v1', kind: 'Infrastructure' },
    name: 'cluster',
  });

  const ucPlatform = Array.isArray(upgradeConfigs) && upgradeConfigs.length > 0
    ? (upgradeConfigs[0].spec?.type as 'OSD' | 'ARO' | undefined)
    : undefined;
  const infraType = infra?.status?.platformStatus?.type;
  const infraPlatform = infraType === 'Azure' ? 'ARO' as const : infraType === 'AWS' ? 'OSD' as const : undefined;
  const detectedPlatform = ucPlatform ?? infraPlatform;

  const currentVersion = cv?.status?.desired?.version
    ?? cv?.status?.history?.find(h => h.state === 'Completed')?.version
    ?? '';
  const currentChannel      = cv?.spec?.channel ?? '';
  const availableUpdates    = cv?.status?.availableUpdates ?? [];
  const conditionalUpdates  = cv?.status?.conditionalUpdates ?? [];

  const channelMatch = currentChannel.match(/^(stable|fast|candidate|eus)-(\d+\.\d+)$/);
  const channelXY    = channelMatch?.[2] ?? '';

  // Fetch available versions for a channel (with client-side caching)
  const fetchAvailableVersions = React.useCallback(async (targetChannel: string, forceRefresh = false) => {
    if (!targetChannel || !CHANNEL_RE.test(targetChannel)) return;

    setFetchingVersions(true);
    setVersionsFetchError(null);

    try {
      const url = `${AVAILABLE_VERSIONS_API}?channel=${encodeURIComponent(targetChannel)}${forceRefresh ? '&refresh=true' : ''}`;
      const response = await consoleFetch(url);
      const data: AvailableVersionsData = await response.json();
      setDynamicVersions(data);
    } catch (e: unknown) {
      const errorMsg = e instanceof Error ? e.message : 'Failed to fetch available versions';
      setVersionsFetchError(errorMsg);
      console.error('Error fetching available versions:', e);
    } finally {
      setFetchingVersions(false);
    }
  }, []);

  // Fetch versions when channel changes
  React.useEffect(() => {
    if (!isOpen || !channel || useCustomChannel) return;

    // Check if cached data is still valid
    if (dynamicVersions?.channel === channel) {
      const expiresAt = new Date(dynamicVersions.cacheExpiresAt).getTime();
      if (Date.now() < expiresAt) {
        // Cache is still valid
        return;
      }
    }

    // Fetch fresh data
    fetchAvailableVersions(channel, false);
  }, [isOpen, channel, useCustomChannel, dynamicVersions, fetchAvailableVersions]);

  React.useEffect(() => {
    if (!isOpen) return;
    if (initialValues) {
      setVersion(initialValues.version);
      setChannel(initialValues.channel);
      setPlatform(detectedPlatform ?? initialValues.platform);
      setDescription(initialValues.description);
      setUseCustomVersion(true);
      setUseCustomChannel(true);
    } else {
      if (currentChannel && !channel) setChannel(currentChannel);
      if (detectedPlatform) setPlatform(detectedPlatform);
    }
  }, [isOpen, initialValues, detectedPlatform, currentChannel, channel]);

  const versionError = version && !SEMVER_RE.test(version) ? 'Version must be in X.Y.Z format (e.g. 4.22.22)' : '';
  const channelError = channel && !CHANNEL_RE.test(channel) ? 'Channel must be in {type}-X.Y format (e.g. stable-4.22)' : '';
  const canSubmit    = version && channel && !versionError && !channelError && !submitting;

  const resetForm = () => {
    setVersion('');
    setChannel(currentChannel);
    setDescription('');
    setUseCustomVersion(false);
    setUseCustomChannel(false);
    setError(null);
    setVersionsFetchError(null);
    // Keep dynamicVersions cached for reuse
  };

  const handleClose = () => { resetForm(); onClose(); };

  const handleSubmit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      await consoleFetch(UPGRADEDIAGS_API, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          targetVersion: version,
          targetChannel: channel,
          platform,
          ...(description ? { description } : {}),
        }),
      });
      resetForm();
      onClose();
      onCreated();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to create upgrade diags check');
    } finally {
      setSubmitting(false);
    }
  };

  if (!isOpen) return null;

  // Inline panel — avoids PF Modal's React portal which breaks inside the
  // OpenShift console's plugin iframe (renders outside the visible window).
  return (
    <div style={{
      marginTop: 16,
      padding: '20px 24px',
      border: `1px solid ${T.border}`,
      borderRadius: T.radiusLg,
      background: T.bgSecondary,
      boxShadow: T.shadow,
    }}>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 20 }}>
        <div>
          <div style={{ fontWeight: 700, fontSize: 18 }}>
            {initialValues ? 'Re-run Diagnostic Check' : 'Run Diagnostic Check'}
          </div>
          {currentVersion && (
            <div style={{ marginTop: 4, fontSize: 13, color: T.textSubtle }}>
              Current cluster version: <strong style={{ color: T.textSubtle }}>{currentVersion}</strong>
            </div>
          )}
        </div>
        <Button variant="plain" aria-label="Close form" onClick={handleClose} style={{ color: T.textSubtle }}>
          <TimesIcon />
        </Button>
      </div>

      {/* Error message */}
      {error && (
        <div style={{
          padding: '10px 14px', marginBottom: 16,
          background: T.dangerBg,
          borderLeft: `3px solid ${T.danger}`,
          borderRadius: T.radiusSm, color: T.danger, fontSize: 13,
        }}>
          {error}
        </div>
      )}

      <Form>
        {/* ── Version ── */}
        <FormGroup label="Target Version" isRequired fieldId="version">
          {/* Show loading state */}
          {(fetchingVersions || (!cvLoaded && !dynamicVersions)) && (
            <><Spinner size="sm" /> Loading available versions…</>
          )}

          {/* Show version dropdown */}
          {cvLoaded && !useCustomVersion && (
            <>
              <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
                <div style={{ flex: 1 }}>
                  <FormSelect
                    id="version"
                    value={version}
                    onChange={(_e, v) => {
                      if (v === '__custom__') { setUseCustomVersion(true); setVersion(''); }
                      else setVersion(v);
                    }}
                    aria-label="Select target version"
                    isDisabled={fetchingVersions}
                  >
                    <FormSelectOption value="" label="Select a version…" isPlaceholder />
                    {currentVersion && (
                      <FormSelectOption value={currentVersion} label={`${currentVersion} (current)`} isDisabled />
                    )}

                    {/* Use dynamic versions if available, otherwise fall back to CVO data */}
                    {dynamicVersions && dynamicVersions.channel === channel ? (
                      <>
                        {dynamicVersions.availableUpdates.length > 0 && (
                          <>
                            <FormSelectOption value="" label="— Available Updates —" isDisabled />
                            {dynamicVersions.availableUpdates.map(u => (
                              <FormSelectOption key={u.version} value={u.version} label={u.version} />
                            ))}
                          </>
                        )}
                        {dynamicVersions.conditionalUpdates.length > 0 && (
                          <>
                            <FormSelectOption value="" label="— Conditional Updates —" isDisabled />
                            {dynamicVersions.conditionalUpdates.map(u => (
                              <FormSelectOption
                                key={u.version}
                                value={u.version}
                                label={`${u.version}${u.risks && u.risks.length > 0 ? ` — ${u.risks[0]}` : ''}`}
                              />
                            ))}
                          </>
                        )}
                      </>
                    ) : (
                      <>
                        {availableUpdates.length > 0 && (
                          <>
                            <FormSelectOption value="" label="— Available Updates —" isDisabled />
                            {availableUpdates.map(u => (
                              <FormSelectOption key={u.version} value={u.version} label={u.version} />
                            ))}
                          </>
                        )}
                        {conditionalUpdates.length > 0 && (
                          <>
                            <FormSelectOption value="" label="— Conditional Updates —" isDisabled />
                            {conditionalUpdates.map(u => (
                              <FormSelectOption
                                key={u.release.version}
                                value={u.release.version}
                                label={`${u.release.version}${u.risks?.[0]?.name ? ` — ${u.risks[0].name}` : ''}`}
                              />
                            ))}
                          </>
                        )}
                      </>
                    )}

                    <FormSelectOption value="" label="—" isDisabled />
                    <FormSelectOption value="__custom__" label="Custom version…" />
                  </FormSelect>
                </div>

                {/* Refresh button */}
                {channel && CHANNEL_RE.test(channel) && !useCustomChannel && (
                  <Button
                    variant="secondary"
                    onClick={() => fetchAvailableVersions(channel, true)}
                    isDisabled={fetchingVersions}
                    icon={<SyncIcon />}
                    aria-label="Refresh available versions"
                  >
                    {fetchingVersions ? 'Refreshing...' : 'Refresh'}
                  </Button>
                )}
              </div>

              <FormHelperText>
                <HelperText><HelperTextItem variant={versionsFetchError ? 'error' : 'default'}>
                  {versionsFetchError ? (
                    `Error: ${versionsFetchError}`
                  ) : dynamicVersions && dynamicVersions.channel === channel ? (
                    <>
                      Versions for <strong>{dynamicVersions.channel}</strong>
                      {' · '}
                      {dynamicVersions.availableUpdates.length + dynamicVersions.conditionalUpdates.length === 0 ? (
                        'No updates available'
                      ) : (
                        `${dynamicVersions.availableUpdates.length + dynamicVersions.conditionalUpdates.length} available`
                      )}
                      {' · '}
                      Cached {new Date(dynamicVersions.cachedAt).toLocaleTimeString()}
                    </>
                  ) : availableUpdates.length === 0 && conditionalUpdates.length === 0 && currentVersion ? (
                    'No updates available from CVO — use "Custom version…" to test any target.'
                  ) : (
                    'Select from available upgrade targets, or enter a custom version.'
                  )}
                </HelperTextItem></HelperText>
              </FormHelperText>
            </>
          )}

          {cvLoaded && (cvError || useCustomVersion) && (
            <>
              {cvError && (
                <div style={{ padding: '8px 12px', marginBottom: 8, background: T.warningBg, borderLeft: `3px solid ${T.warning}`, borderRadius: T.radiusSm, fontSize: 13, color: T.warning }}>
                  Unable to load available versions — enter manually
                </div>
              )}
              <TextInput
                id="version-custom"
                value={version}
                onChange={(_e, v) => setVersion(v)}
                placeholder="e.g. 4.22.22"
                validated={versionError ? 'error' : 'default'}
                isRequired
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant={versionError ? 'error' : 'default'}>
                    {versionError || (
                      useCustomVersion
                        ? <Button variant="link" isInline onClick={() => { setUseCustomVersion(false); setVersion(''); }}>← Back to version list</Button>
                        : 'Enter the target version in X.Y.Z format'
                    )}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </>
          )}
        </FormGroup>

        {/* ── Channel ── */}
        <FormGroup label="Target Channel" isRequired fieldId="channel">
          {!useCustomChannel ? (
            <>
              <FormSelect
                id="channel"
                value={channel}
                onChange={(_e, v) => {
                  if (v === '__custom__') { setUseCustomChannel(true); setChannel(''); }
                  else setChannel(v);
                }}
                aria-label="Select target channel"
              >
                <FormSelectOption value="" label="Select a channel…" isPlaceholder />
                {currentChannel && (
                  <FormSelectOption value={currentChannel} label={`${currentChannel} (current)`} />
                )}
                {channelXY && (
                  <>
                    <FormSelectOption value="" label="— Sibling Channels —" isDisabled />
                    {['stable', 'fast', 'candidate'].map(t => {
                      const c = `${t}-${channelXY}`;
                      return c !== currentChannel
                        ? <FormSelectOption key={c} value={c} label={c} />
                        : null;
                    })}
                  </>
                )}
                <FormSelectOption value="" label="—" isDisabled />
                <FormSelectOption value="__custom__" label="Custom channel…" />
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant={channelError ? 'error' : 'default'}>
                    {channelError || 'stable (production), fast (early access), candidate (pre-release)'}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </>
          ) : (
            <>
              <TextInput
                id="channel-custom"
                value={channel}
                onChange={(_e, v) => setChannel(v)}
                placeholder="e.g. stable-4.22"
                validated={channelError ? 'error' : 'default'}
                isRequired
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant={channelError ? 'error' : 'default'}>
                    {channelError || (
                      <Button variant="link" isInline onClick={() => { setUseCustomChannel(false); setChannel(currentChannel); }}>← Back to channel list</Button>
                    )}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </>
          )}
        </FormGroup>

        {/* ── Platform ── */}
        <FormGroup label="Platform" isRequired fieldId="platform">
          <FormSelect
            id="platform"
            value={platform}
            onChange={detectedPlatform ? undefined : ((_e, v) => setPlatform(v as 'OSD' | 'ARO'))}
            isDisabled={!!detectedPlatform}
            aria-label="Platform type"
          >
            <FormSelectOption value="OSD" label="OpenShift Dedicated (OSD)" />
            <FormSelectOption value="ARO" label="Azure Red Hat OpenShift (ARO)" />
          </FormSelect>
          {detectedPlatform && (
            <FormHelperText>
              <HelperText><HelperTextItem>
                Detected from cluster configuration
              </HelperTextItem></HelperText>
            </FormHelperText>
          )}
        </FormGroup>

        {/* ── Description (optional) ── */}
        <FormGroup label="Description" fieldId="description">
          <TextInput
            id="description"
            value={description}
            onChange={(_e, v) => setDescription(v)}
            placeholder="e.g. Pre-upgrade planning, in-progress upgrade diagnosis, post-failure analysis"
          />
          <FormHelperText>
            <HelperText><HelperTextItem>Optional label stored with this diagnostic run — useful for identifying its purpose later</HelperTextItem></HelperText>
          </FormHelperText>
        </FormGroup>
      </Form>

      {/* Actions */}
      <div style={{ marginTop: 24, display: 'flex', gap: 12, alignItems: 'center' }}>
        <Button variant="primary" isLoading={submitting} isDisabled={!canSubmit} onClick={handleSubmit} style={{ borderRadius: T.radiusSm }}>
          Run Diagnostic Check
        </Button>
        <Button variant="link" onClick={handleClose} isDisabled={submitting}>
          Cancel
        </Button>
      </div>
    </div>
  );
};
