# MUO Agent Handoff — Full Context Restore

**Date:** 2026-11-12 (last updated by remote agent)
**Branch:** `<your-branch-name>`
**Latest commit:** `9184f70`
**Remote agent fork:** `<remote-fork>/managed-upgrade-operator`  
**Local agent fork:** `<your-fork>/managed-upgrade-operator`  
**Upstream (read-only reference):** `openshift/managed-upgrade-operator`

---

## Recent Changes (since last deploy)

| Commit | What changed |
|--------|-------------|
| `9184f70` | Gap A/B/C: `StuckDrainingNodes` check with per-pod root cause (PDB/finalizer/terminating), `unhealthyPodEvictionPolicy` surfaced in all PDB findings, `--disable-eviction` guidance (MCS-251) |
| `2713fd5` | `namespacePlatformCache` — dynamic namespace owner classification via managedFields (Hive, addon-operator, etc.); replaces static prefix-only check in `StuckTerminatingPods` |
| `048dc2c` | Gap 1: `PodDisruptionBudgetsDrainSafety` (MCS-252 four-case predicate); Gap 3: `UpgradeConfigConsistency` (UpgradeConfigSyncFailure detection) |
| `800bb23` | 7 new checks from OHSS MCS-48 review: PausedMachineConfigPool, WorkerNodeReadiness, StuckTerminatingPods, PendingCSRs, InstalledOperatorMaxVersion, ConsolePluginReferences, ClusterAutoscalerConfig |

**Check catalog: was 17, now 27.**

---

## Two-Agent Collaboration Model

| Agent | Where | Role |
|-------|-------|------|
| **Remote agent** (this sandbox) | `/sandbox/muo-workspace/` | Writes Go/TS code, commits, pushes to remote fork |
| **Local agent** (local machine) | Clone of your fork | Pulls from remote, builds images, deploys to cluster, tests, writes results to `/sandbox/ai-status/` |

**Communication:** Local agent drops status files in `/sandbox/ai-status/`. Remote agent reads them between coding sessions.

---

## Cluster & Registry

| Resource | Value |
|----------|-------|
| Cluster console | `console-openshift-console.apps.<cluster-name>.s1.devshift.org` |
| MUO operator namespace | `openshift-managed-upgrade-operator` |
| Operator image registry | `quay.io/<your-username>/managed-upgrade-operator:<TAG>` |
| Plugin image registry | `quay.io/<your-username>/muo-pko:<TAG>` (the PKO bundle image) |
| PKO resource | `ClusterPackage/managed-upgrade-operator` |

---

## Repository Setup (remote agent)

```bash
git clone "https://x-access-token:${GH_TOKEN}@github.com/<remote-fork>/managed-upgrade-operator.git" muo-workspace
cd muo-workspace
git checkout <your-branch-name>
git config user.email "<your-email>"
git config user.name "<your-name>"
```

Push after changes:
```bash
git remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/<remote-fork>/managed-upgrade-operator.git"
git push origin <your-branch-name>
```

---

## What This Branch Adds (vs upstream master)

### Feature: UpgradeDiagsCheck CRD + Controller

A new subsystem (`upgradediags`) sits alongside the main MUO operator as a **separate container** (`args: [upgradediags]`). It:

1. Accepts `POST /api/v1/upgrade-diags` from the console plugin (via OpenShift console proxy `muo-api`)
2. Creates an internal `UpgradeDiagsCheck` CR in `openshift-managed-upgrade-operator`
3. Controller reconciles the CR, runs registered checks, writes results back to CR status
4. `GET /api/v1/upgrade-diags/{id}` polls for results
5. `GET /api/v1/upgrade-status` returns a live upgrade-state snapshot (no CR)
6. `GET /api/v1/upgrade-status/history` returns CVO upgrade history (no CR)

### Feature: OpenShift Dynamic Console Plugin

A webpack/Module Federation console plugin shows in the OpenShift Console under **Administration → Upgrade Diagnostics**. It lets users:
- View all `UpgradeDiagsCheck` CRs in a live-updating table
- Create new checks via a form (version/channel dropdowns from ClusterVersion)
- Expand rows to see per-check results

---

## Key Files Changed vs Upstream

### Go — API types

| File | Description |
|------|-------------|
| `api/v1alpha1/upgradediagscheck_types.go` | CRD types: `UpgradeDiagsCheck`, `UpgradeDiagsCheckSpec/Status`, `DiagsCheckResult` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Auto-generated deep-copy (updated for new types) |

### Go — Controller

| File | Description |
|------|-------------|
| `controllers/upgradediags/upgradediags_controller.go` | Reconciler: watches `UpgradeDiagsCheck` CRs, runs registered checks |
| `controllers/upgradediags/http_service.go` | TLS HTTP server: auth, routing, CR CRUD, upgrade-status routes |
| `controllers/upgradediags/upgrade_status.go` | **NEW** — live upgrade status snapshot + history (no CR) |
| `controllers/upgradediags/check_interface.go` | `DiagnosticsCheck` interface |
| `controllers/upgradediags/checks.go` | `var registeredChecks` — the check registry |
| `controllers/upgradediags/upgradediags_controller_test.go` | Ginkgo tests |

### Go — Entry point

| File | Change |
|------|--------|
| `main.go` | Dispatches `os.Args[1] == "upgradediags"` → `runUpgradeDiags()` which starts controller + HTTP server |

### Deploy — PKO manifests (`deploy_pko/`)

| File | Description |
|------|-------------|
| `manifest.yaml` | PKO PackageManifest; phases: crds→namespace→rbac→certs→deploy; config params: `image`, `pluginImage` |
| `Deployment-managed-upgrade-operator.yaml.gotmpl` | Main MUO deployment; includes `upgradediags` sidecar container |
| `Deployment-upgradediags.yaml.gotmpl` | Separate plugin pod serving console assets on port 9443 |
| `Service-managed-upgrade-operator-upgradediags.yaml` | Port 8443, TLS (OpenShift serving cert), for the HTTP API |
| `Service-upgradediags.yaml` | Port 9443, for the console plugin asset server |
| `ConsolePlugin-managed-upgrade-operator.yaml` | Proxy alias `muo-api` → `managed-upgrade-operator-upgradediags:8443` |
| `CustomResourceDefinition-upgradediagschecks.upgrade.managed.openshift.io.yaml` | CRD for `UpgradeDiagsCheck` |
| `ClusterRole-managed-upgrade-operator.yaml` | RBAC for operator SA |
| `Role-upgradediags-reader.yaml` | Allows users to GET/LIST upgradediagschecks |
| `Role-ocm-agent-upgradediags.yaml` | Allows OCM agent to call the HTTP API |

### Console Plugin (`consoleplugin/src/components/`)

| File | Description |
|------|-------------|
| `UpgradeDiagsPage.tsx` | Main page: live table of `UpgradeDiagsCheck` CRs via `useK8sWatchResource` |
| `CreateDiagsCheckForm.tsx` | Modal form: POSTs to `/api/v1/upgrade-diags` via console proxy |
| `UpgradeDiagsDetail.tsx` | Expandable detail row per check |
| `StatusBadge.tsx` | Label component for check status |
| `../plugin.json` | Plugin metadata; nav entry `UpgradeDiagsPage` |
| `../console-extensions.json` | Route `/upgrade-diagnostics`, component `UpgradeDiagsPage` |

---

## Naming Conventions (critical — many things renamed)

| Old name | New name |
|----------|----------|
| `readiness` (package/subsystem) | `upgradediags` |
| `UpgradeReadinessCheck` (CRD kind) | `UpgradeDiagsCheck` |
| `upgradereadinesschecks` (CRD plural) | `upgradediagschecks` |
| `urc` (CRD short name) | `udc` |
| `ReadinessCheck` (interface) | `DiagnosticsCheck` |
| `ReadinessHTTPServer` | `UpgradeDiagsHTTPServer` |
| `/api/v1/readiness-checks` | `/api/v1/upgrade-diags` |
| `readiness.managed.openshift.io/` (labels) | `upgradediags.managed.openshift.io/` |
| `READINESS_TLS_CERT/KEY` (env vars) | `UPGRADEDIAGS_TLS_CERT/KEY` |
| `/etc/readiness-tls/` (mount path) | `/etc/upgradediags-tls/` |
| `muo-readiness-tls` (secret name) | `muo-upgradediags-tls` |
| `ReadinessController` (feature gate) | **Removed** (gate eliminated entirely) |
| CR prefix `readiness-XXXXX` | Auto-generated `udc-{version}-{platform}-XXXXX` |

---

## HTTP API Reference

Base URL (via console proxy): `/api/plugins/managed-upgrade-operator/api/v1/`  
Direct TLS (from cluster): `https://managed-upgrade-operator-upgradediags.openshift-managed-upgrade-operator.svc:8443/api/v1/`

All endpoints require: `Authorization: Bearer <user-token>`  
Auth is enforced via TokenReview + SubjectAccessReview against `upgradediagschecks` resource.

### POST /api/v1/upgrade-diags
Create a new diagnostics run. Returns `202 Accepted` with `requestId`.

```json
{
  "targetVersion": "4.22.22",
  "targetChannel": "fast-4.22",
  "platform": "OSD",
  "defaultCheckTimeoutSeconds": 60,
  "overallTimeoutSeconds": 300,
  "checksToRun": [],
  "description": "optional",
  "namePrefix": ""
}
```

### GET /api/v1/upgrade-diags
List all diagnostics runs.

### GET /api/v1/upgrade-diags/{requestId}
Poll status of a specific run.

### DELETE /api/v1/upgrade-diags/{requestId}
Delete a completed run CR.

### GET /api/v1/upgrade-diags/checks
Catalog of registered checks (no auth required).

### GET /api/v1/upgrade-status
**Live upgrade state snapshot — no CR created.**

```json
{
  "phase": "operators",
  "targetVersion": "4.22.22",
  "currentVersion": "4.21.8",
  "cvoMessage": "...",
  "startedAt": "...",
  "elapsedSeconds": 1234,
  "controlPlane": {
    "phase": "complete",
    "startedAt": "...", "completedAt": "...", "elapsedSeconds": 450
  },
  "clusterOperators": {
    "phase": "progressing",
    "total": 32, "available": 28, "progressing": 3, "degraded": 1,
    "operators": [
      {
        "name": "authentication",
        "phase": "progressing",
        "version": "4.22.22",
        "progressingSince": "...",
        "elapsedInPhaseSeconds": 180,
        "message": "..."
      }
    ]
  },
  "machinePools": [
    {
      "name": "master",
      "phase": "updated",
      "total": 3, "updated": 3, "ready": 3,
      "startedAt": "...", "completedAt": "...", "elapsedSeconds": 420,
      "nodes": [
        { "name": "ip-10-0-1-1", "phase": "done", "currentConfig": "...", "desiredConfig": "..." }
      ]
    },
    {
      "name": "worker",
      "phase": "updating",
      "total": 6, "updated": 2, "ready": 2, "unavailable": 1,
      "nodes": [...]
    }
  ],
  "blockers": [
    {
      "type": "operator-progressing-stalled",
      "component": "authentication",
      "message": "Progressing for 25m: ...",
      "since": "...",
      "elapsedSeconds": 1500
    }
  ]
}
```

**Phase values:** `not-started` | `pre-checks` | `control-plane` | `operators` | `worker-upgrade` | `complete` | `failed`

**Blocker types:** `cvo-degraded` | `operator-degraded` | `operator-progressing-stalled` | `mcp-degraded` | `node-stalled`

### GET /api/v1/upgrade-status/history
CVO upgrade history from `ClusterVersion.status.history`.

```json
{
  "total": 3,
  "history": [
    { "version": "4.21.8", "state": "Completed", "startedAt": "...", "completedAt": "..." },
    { "version": "4.20.15", "state": "Completed", "startedAt": "...", "completedAt": "..." }
  ]
}
```

---

## Registered Checks (27 total as of commit 9184f70)

### Upgrade eligibility
| Check Name | What It Tests |
|------------|---------------|
| `ClusterVersion` | CVO ClusterVersion CR is accessible and parseable |
| `UpgradeTarget` | Target version exists in available updates |
| `UpgradeTargetExtended` | Detailed upgrade path: Cincinnati vs CVO cache, conditional risks |
| `UpgradeInFlight` | No other upgrade already in progress |
| `UpgradeableCondition` | CVO Upgradeable=True |
| `AdminAcks` | All required admin-acks present |
| `PullSecretValid` | pull-secret in openshift namespace is valid JSON |

### Cluster health gates
| Check Name | What It Tests |
|------------|---------------|
| `ClusterOperators` | All ClusterOperators show Available=True |
| `ClusterOperatorsExtended` | Operator details with phase/elapsed timing |
| `CriticalAlerts` | No critical alerts firing in core namespaces |
| `CriticalAlertsExtended` | Detailed alert info with descriptions |

### Node readiness
| Check Name | What It Tests |
|------------|---------------|
| `WorkerNodeReadiness` | Worker nodes not in Ready state before upgrade (OHSS Case 10) |
| `ManuallyCordonedNodes` | Nodes cordoned outside of an upgrade |
| `ManuallyCordonedNodesExtended` | Details of cordoned nodes |
| `NodeTaints` | Unexpected taints blocking scheduling |
| `NodeTaintsExtended` | Full taint details |
| `PendingCSRs` | Unapproved CertificateSigningRequests blocking new nodes (OHSS Case 3) |

### Workload / drain blockers
| Check Name | What It Tests |
|------------|---------------|
| `PodDisruptionBudgets` | MUO's HealthCheckPDB gate (DVO metrics + direct PDB check) |
| `PodDisruptionBudgetsMasterInfraExtended` | PDBs covering running pods on master/infra nodes |
| `PodDisruptionBudgetsDrainSafety` | Per-PDB currentHealthy/disruptionsAllowed/unhealthyPodEvictionPolicy — MCS-252 four-case safety predicate |
| `StuckDrainingNodes` | Cordoned nodes with per-pod root cause: PDB block, finalizer hold, long Terminating (MCS-251/253) |
| `StuckTerminatingPods` | Pods stuck Terminating >5min across all namespaces (OHSS Case 7) |

### Configuration compatibility
| Check Name | What It Tests |
|------------|---------------|
| `PausedMachineConfigPool` | MCPs with spec.paused=true blocking worker rollout (OHSS Case 3) |
| `InstalledOperatorMaxVersion` | OLM CSVs with olm.maxOpenShiftVersion < target (OHSS Case 5) |
| `ConsolePluginReferences` | ConsolePlugin CRs referencing non-existent Services (OHSS Case 5) |
| `ClusterAutoscalerConfig` | Invalid utilizationThreshold or maxNodesTotal (OHSS Case 5) |
| `UpgradeConfigConsistency` | MUO UpgradeConfig desired version vs CVO desired version — config sync failure detection (MCS-48 Case 9) |

---

## Future: Separate Diagnostics Package (opt-in)

### Intent

The diagnostics subsystem is designed to eventually ship as a separate,
opt-in PKO package (`managed-upgrade-operator-diagnostics`) that customers
or SREs install alongside core MUO. Installing the package would bring:

- `Deployment-upgradediags` — the HTTP server container
- `CustomResourceDefinition-upgradediagschecks` — the UpgradeDiagsCheck CRD
- `ClusterRole/Binding-managed-upgrade-operator-diagnostics` — diagnostic RBAC only
- `ConsolePlugin` CR — the Upgrade Diagnostics tab
- `Role/RoleBinding-upgradediags-reader` — cluster-admin tab access
- `Service/Route-*-upgradediags` — network exposure

Without this package installed, the `ReconcileUpgradeDiagsCheck` controller
registered in the main MUO binary sits idle — it watches for `UpgradeDiagsCheck`
CRs that will never be created because the HTTP service and ConsolePlugin are
not present. No CRs are created, no checks run, no RBAC is used. The main
upgrade path is completely unaffected.

### What enables this cleanly today

- Diagnostic RBAC is already in a **separate ClusterRole**
  (`managed-upgrade-operator-diagnostics`) — not merged into the upgrade ClusterRole
- The `upgradediags` container is already a **separate Deployment**
- The `ConsolePlugin` CR, reader Role/Binding, and CRD are all **standalone manifests**

### Follow-up work required before packaging

**Binary split (most significant):** The `ReconcileUpgradeDiagsCheck` reconciler
currently runs inside the main MUO binary using the main manager's client, scheme,
and leader election. For full decoupling it needs to move into the `upgradediags`
binary with its own `ctrl.NewManager` setup. Until then the reconciler is an idle
goroutine in the main binary — harmless, but it means the main operator binary has
a compile-time dependency on the diagnostics package.

Steps for the follow-up PR:
1. Add `ctrl.NewManager` + scheme registration to the `upgradediags` entrypoint
2. Register `ReconcileUpgradeDiagsCheck` there instead of `main.go`
3. Remove the registration from `main.go`
4. Decide ServiceAccount: share the existing MUO SA or create a dedicated
   `managed-upgrade-operator-upgradediags` SA with only the diagnostic RBAC

**Do NOT** merge diagnostic RBAC back into the main ClusterRole while this split
is pending — the separation established in this branch is the correct boundary.

---

## Planned Next Phases

### Phase B — Console: Text/Table upgrade status view
- New tab or section in `UpgradeDiagsPage` showing `GET /api/v1/upgrade-status` data
- Table of operators with phase, elapsed time, status badge
- Table of machine pools with node counts

### Phase C — Console: React Flow graphical upgrade flowchart
- Install `reactflow` npm package
- Canvas with phase nodes: Pre-checks → Control Plane → Operators (expandable per-operator) → Workers (expandable per-MCP/node) → Complete
- Color: green=done, yellow=progressing, red=degraded, gray=not-started
- Click node to open side drawer with full details

### Phase D — Drill-down drawers
- Per-operator drawer: conditions, versions, events
- Per-node drawer: MachineConfig state, annotations, pod disruption info

---

## Build Instructions (local agent)

```bash
cd /path/to/muo-workspace   # cloned from your fork

# 1. Sync latest from remote fork
git remote add remote-fork https://github.com/<remote-fork>/managed-upgrade-operator.git 2>/dev/null || true
git fetch remote-fork
git checkout <your-branch-name>
git merge remote-fork/<your-branch-name> --ff-only

# 2. Set image tag
TAG="upgradestatus-$(date +%m%d%H%M)"

# 3. Build operator image (contains both main MUO binary and upgradediags binary)
podman build -f build/Dockerfile -t quay.io/<your-username>/managed-upgrade-operator:$TAG .
podman push quay.io/<your-username>/managed-upgrade-operator:$TAG

# 4. Build PKO bundle image
# PKO bundle = OCI image whose layers are the deploy_pko/ manifest files
mkdir -p /tmp/pko-build
cp -r deploy_pko/* /tmp/pko-build/
cat > /tmp/pko-build/Dockerfile <<'EOF'
FROM scratch
COPY . /package/
EOF
podman build -t quay.io/<your-username>/muo-pko:$TAG /tmp/pko-build/
podman push quay.io/<your-username>/muo-pko:$TAG

echo "Built TAG=$TAG — use this in deploy step"
```

---

## Deploy Instructions (local agent)

```bash
TAG="<tag from build step>"
NS="openshift-managed-upgrade-operator"

# 5. Remove old CRD if it still has the old name (upgradereadinesschecks)
oc get crd upgradereadinesschecks.upgrade.managed.openshift.io 2>/dev/null && \
  oc delete crd upgradereadinesschecks.upgrade.managed.openshift.io

# 6. Delete and recreate ClusterPackage to pick up new images
oc delete clusterpackage managed-upgrade-operator --ignore-not-found
sleep 5

oc apply -f - <<EOF
apiVersion: package-operator.run/v1alpha1
kind: ClusterPackage
metadata:
  name: managed-upgrade-operator
spec:
  image: quay.io/<your-username>/muo-pko:$TAG
  config:
    image: quay.io/<your-username>/managed-upgrade-operator:$TAG
    pluginImage: quay.io/<your-username>/managed-upgrade-operator:$TAG
EOF

# 7. Watch rollout (should reach Available=True in ~3 minutes)
watch oc get clusterpackage managed-upgrade-operator

# 8. Verify pods
oc get pods -n $NS
# Expected: managed-upgrade-operator-* Running, upgradediags-* Running

# 9. Enable console plugin (one-time if not already done)
oc patch console.operator cluster \
  --type=merge \
  -p '{"spec":{"plugins":["managed-upgrade-operator"]}}'
```

---

## Test Instructions (local agent)

### Test A: HTTP API — upgrade-diags

```bash
NS="openshift-managed-upgrade-operator"
SVC="managed-upgrade-operator-upgradediags"
TOKEN=$(oc whoami -t)

# Port-forward (or use Route if present)
oc port-forward -n $NS svc/$SVC 8443:8443 &
PF_PID=$!
sleep 2

# Check catalog (no auth needed)
curl -sk https://localhost:8443/api/v1/upgrade-diags/checks | jq '.total'
# Expected: 27

# Create a diagnostics run
curl -sk -X POST https://localhost:8443/api/v1/upgrade-diags \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targetVersion":"4.22.22","targetChannel":"fast-4.22","platform":"OSD"}' | jq .
# Expected: {"requestId":"...","status":"accepted","statusUrl":"/api/v1/upgrade-diags/..."}

# Poll for results (wait ~30-60s)
REQUEST_ID="<id from above>"
curl -sk https://localhost:8443/api/v1/upgrade-diags/$REQUEST_ID \
  -H "Authorization: Bearer $TOKEN" | jq '{status, overallStatus, checksTotal, checksPassed, checksFailed}'

kill $PF_PID
```

### Test B: HTTP API — upgrade-status (NEW)

```bash
TOKEN=$(oc whoami -t)
oc port-forward -n $NS svc/$SVC 8443:8443 &
PF_PID=$!; sleep 2

# Live upgrade status snapshot
curl -sk https://localhost:8443/api/v1/upgrade-status \
  -H "Authorization: Bearer $TOKEN" | jq '{phase, targetVersion, currentVersion, elapsedSeconds, blockers}'

# Upgrade history
curl -sk https://localhost:8443/api/v1/upgrade-status/history \
  -H "Authorization: Bearer $TOKEN" | jq '.history[0:3]'

kill $PF_PID
```

**Expected upgrade-status results:**
- `phase`: likely `not-started` if no upgrade is in progress
- `targetVersion`: empty if no `DesiredUpdate` is set on ClusterVersion
- `currentVersion`: should match `oc get clusterversion version -o jsonpath='{.status.desired.version}'`
- `blockers`: empty array if cluster is healthy
- History: list of past upgrades with start/completion times

### Test C: Console Plugin — Upgrade Diagnostics page

1. Open `console-openshift-console.apps.<cluster-name>.s1.devshift.org`
2. Navigate to **Administration → Upgrade Diagnostics**
3. Verify table loads (may be empty if no checks run yet)
4. Click **Run New Diagnostics**
5. Select version and channel (dropdowns should auto-populate from ClusterVersion)
6. Click **Run** — form should close, row should appear in table
7. Wait 30s, row status should update to Completed/Passed/Failed

### Test D: Check CRs from CLI

```bash
oc get upgradediagschecks -n openshift-managed-upgrade-operator
# or short name:
oc get udc -n openshift-managed-upgrade-operator
# Should show CR with name like udc-4-22-22-osd-XXXXX
```

---

## Reporting Results (local agent)

Write results to `/sandbox/ai-status/` with descriptive names:

```bash
# Success
echo "PASS" > /sandbox/ai-status/RESULT_UPGRADE_STATUS_API
echo "Phase B ready for implementation" > /sandbox/ai-status/TRIGGER_PHASE_B

# Failure — include enough detail to diagnose
cat > /sandbox/ai-status/RESULT_UPGRADE_STATUS_FAIL <<EOF
Test B failed: upgrade-status returns 500
Error: reading ClusterVersion: <error message>
Pod logs:
$(oc logs -n openshift-managed-upgrade-operator deploy/managed-upgrade-operator -c upgradediags --tail=30)
EOF
```

---

## Known Issues / Watch-Outs

1. **Old CRD conflict:** If `upgradereadinesschecks.upgrade.managed.openshift.io` CRD still exists from a prior deploy, delete it before applying the new `upgradediagschecks` CRD. PKO's collision-protection won't remove the old one automatically.

2. **Image visibility:** PKO bundle image (`muo-pko`) must be publicly readable on Quay.io or PKO will get `UNAUTHORIZED`. Check Quay.io repo visibility settings.

3. **Console plugin cache:** After deploying, the console may serve stale plugin assets for 1-2 minutes. Hard refresh (`Ctrl+Shift+R`) or wait.

4. **consoleFetch is GET-only:** The console proxy (`muo-api`) only supports GET. The plugin uses `consoleFetch` for GET requests and the console proxy routes them to port 8443. POST to create checks goes directly to K8s API via `k8sCreate` — this is intentional, not a bug.

5. **TLS cert timing:** The `muo-upgradediags-tls` secret is created by the OpenShift serving-cert controller after the Service exists. PKO phase ordering (`certs` before `deploy`) handles this, but if the secret is missing, the upgradediags container will log a warning and the HTTP service will be disabled.

---

## Context Restore from Backup

If `/sandbox/backups/` has a `*.tar.gz`:

```bash
ls /sandbox/backups/
tar xzf /sandbox/backups/<latest>.tar.gz -C /tmp/restore/
cat /tmp/restore/AGENT_HANDOFF.md   # this file
```
