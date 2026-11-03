# UpgradePreflightCheck Feature - Complete Testing Guide

This guide provides step-by-step instructions for building, deploying, and testing the UpgradePreflightCheck feature with HTTP service.

## Table of Contents
1. [Prerequisites](#prerequisites)
2. [Checkout PR Branch](#checkout-pr-branch)
3. [Build Container Images](#build-container-images)
4. [Push Images to Registry](#push-images-to-registry)
5. [Deploy to Cluster](#deploy-to-cluster)
6. [Enable Feature Gate](#enable-feature-gate)
7. [Test HTTP Service](#test-http-service)
8. [Verify Preflight Checks](#verify-preflight-checks)
9. [Troubleshooting](#troubleshooting)

---

## Prerequisites

### Required Tools
```bash
# Verify tools are installed
podman --version          # v4.0+
oc version               # OpenShift CLI
gh version               # GitHub CLI (optional)
make --version
go version               # Go 1.26.6+
jq --version             # For JSON parsing
```

### Access Requirements
- **Quay.io account** with push permissions to your repository
- **OpenShift cluster** access with cluster-admin privileges
- **GitHub account** (optional, for viewing PRs)

### Login to Registries
```bash
# Login to Quay.io (for pushing images)
podman login quay.io

# Login to cluster
oc login <cluster-api-url>

# Verify cluster access
oc whoami
oc cluster-info
```

---

## Checkout PR Branch

### Option 1: Checkout from Author's PR (Original)
```bash
cd ~/sandbox
git clone git@github.com:openshift/managed-upgrade-operator.git
cd managed-upgrade-operator

# Add the author's fork as remote
git remote add nephomaniac git@github.com:nephomaniac/managed-upgrade-operator.git
git fetch nephomaniac

# Checkout the PR branch
gh pr checkout 1 --repo nephomaniac/managed-upgrade-operator

# Or manually:
git checkout -b pr-1-test nephomaniac/pr-1-latest
```

### Option 2: Checkout Our Fixed Branch (Recommended)
```bash
cd ~/sandbox
git clone git@github.com:nephomaniac/managed-upgrade-operator.git
cd managed-upgrade-operator

# Checkout our branch with all fixes
git checkout maclark/fix-preflight-http-service

# Verify you're on the correct branch
git branch -v
git log --oneline -5
```

**Expected commits on `maclark/fix-preflight-http-service`:**
- `2a5f29b` - chore: increase max active preflight checks from 2 to 10
- `ec662b4` - feat: warn when PreflightController feature gate is disabled
- `4bc04ad` - feat: add resource management to prevent CR buildup
- `d9b0c31` - fix: HTTP service must set required CRD fields and Service needs PKO phase
- `0451f5b` - fix: add missing RBAC for UpgradePreflightCheck and HTTP service auth

---

## Build Container Images

### Important: Cross-Platform Build

**Critical for macOS users:** Always use `--platform=linux/amd64` when building on macOS to ensure the image runs on OpenShift (Linux) clusters.

### Build Operator Image

```bash
# Set your image tag
export OPERATOR_IMAGE="quay.io/YOUR_USERNAME/managed-upgrade-operator:pr-1-test"

# Build for linux/amd64 (required for cluster deployment)
podman build --platform=linux/amd64 \
  -f build/Dockerfile \
  -t $OPERATOR_IMAGE .

# Verify architecture is correct
podman inspect $OPERATOR_IMAGE | jq '.[0].Architecture'
# Should output: "amd64"

# Verify image size is reasonable
podman images | grep managed-upgrade-operator
# Should be ~200MB
```

**Build time:** 5-10 minutes on macOS (with QEMU emulation), 2-3 minutes on Linux

### Build PKO Package Image

```bash
# Set your PKO package tag
export PKO_IMAGE="quay.io/YOUR_USERNAME/managed-upgrade-operator-pko:pr-1-test"

# Build PKO package (from scratch image with manifests)
cd deploy_pko
podman build -f ../build/Dockerfile.pko \
  -t $PKO_IMAGE .
cd ..

# Verify PKO image created
podman images | grep managed-upgrade-operator-pko
```

**Build time:** < 30 seconds (simple COPY operation)

---

## Push Images to Registry

```bash
# Push operator image
podman push $OPERATOR_IMAGE

# Push PKO package image
podman push $PKO_IMAGE

# Verify images are accessible
podman pull $OPERATOR_IMAGE
podman pull $PKO_IMAGE
```

**Expected output:** Both images should push successfully and be publicly accessible on quay.io

---

## Deploy to Cluster

### Step 1: Prepare Cluster

```bash
# Ensure you're in the correct cluster context
oc cluster-info

# Create or verify namespace exists
oc get namespace openshift-managed-upgrade-operator || \
  oc create namespace openshift-managed-upgrade-operator

# Pause Hive syncs (if using Hive-managed cluster)
oc annotate clustersync cluster-sync \
  -n openshift-managed-upgrade-operator \
  hive.openshift.io/pause=true --overwrite
```

### Step 2: Deploy via PKO ClusterPackage

Create the ClusterPackage manifest:

```bash
cat > /tmp/muo-clusterpackage.yaml << EOF
apiVersion: package-operator.run/v1alpha1
kind: ClusterPackage
metadata:
  name: managed-upgrade-operator
spec:
  image: $PKO_IMAGE
  config:
    image: $OPERATOR_IMAGE
EOF
```

Deploy the ClusterPackage:

```bash
# Remove old deployment if exists
oc delete clusterpackage managed-upgrade-operator 2>/dev/null || true

# Wait for cleanup
sleep 5

# Apply new deployment
oc apply -f /tmp/muo-clusterpackage.yaml

# Watch deployment
oc get clusterpackage managed-upgrade-operator -w
```

**Expected ClusterPackage status:**
```
NAME                       AVAILABLE   PROGRESSING   UNPACKED   INVALID   REVISION   AGE
managed-upgrade-operator   True        False         True                 1          30s
```

### Step 3: Verify Deployment

```bash
# Check ClusterPackage status
oc get clusterpackage managed-upgrade-operator

# Check operator pod
oc get pods -n openshift-managed-upgrade-operator

# Expected output: 1/1 Running
# NAME                                       READY   STATUS    RESTARTS   AGE
# managed-upgrade-operator-xxxxx-yyyyy       1/1     Running   0          2m

# Verify correct image is deployed
oc get deployment -n openshift-managed-upgrade-operator \
  managed-upgrade-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}'

# Should match your $OPERATOR_IMAGE
```

### Step 4: Check Operator Logs

```bash
# View startup logs
oc logs -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator --tail=50

# Look for these key log lines:
# - "Go OS/Arch: linux/amd64"              (correct architecture)
# - "Starting preflight HTTP service"       (HTTP service started)
# - "addr=:8443"                           (listening on port 8443)
# - "Starting Controller"                   (controllers initialized)
```

### Step 5: Verify CRDs Installed

```bash
# Check UpgradePreflightCheck CRD exists
oc get crd upgradepreflightchecks.upgrade.managed.openshift.io

# Check RBAC
oc get clusterrole managed-upgrade-operator -o yaml | \
  grep -A 5 upgradepreflightchecks

# Verify Service exists
oc get svc -n openshift-managed-upgrade-operator \
  managed-upgrade-operator-preflight

# Expected output:
# NAME                                 TYPE        CLUSTER-IP       PORT(S)
# managed-upgrade-operator-preflight   ClusterIP   172.30.x.x       8443/TCP
```

### Step 6: Verify TLS Certificate

```bash
# Check Service CA secret exists
oc get secret -n openshift-managed-upgrade-operator muo-preflight-tls

# Verify certificate validity
oc get secret -n openshift-managed-upgrade-operator muo-preflight-tls \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | \
  openssl x509 -noout -dates

# Should show valid dates (1-2 years)
```

---

## Enable Feature Gate

The PreflightController requires a feature gate to be enabled.

### Check Current Config

```bash
# View current ConfigMap
oc get configmap -n openshift-managed-upgrade-operator \
  managed-upgrade-operator-config -o yaml | \
  grep -A 5 "featureGate:"
```

### Enable PreflightController

```bash
# Get current config
oc get configmap -n openshift-managed-upgrade-operator \
  managed-upgrade-operator-config -o yaml > /tmp/muo-config.yaml

# Edit the config
# Add "PreflightController" to the featureGate.enabled list:
#
# featureGate:
#   enabled:
#   - PreHealthCheck
#   - ServiceLogNotification
#   - PreflightController          # <-- ADD THIS LINE

# Apply updated config
oc apply -f /tmp/muo-config.yaml

# Restart operator to pick up new config
oc rollout restart deployment/managed-upgrade-operator \
  -n openshift-managed-upgrade-operator

# Wait for new pod
oc rollout status deployment/managed-upgrade-operator \
  -n openshift-managed-upgrade-operator

# Verify feature gate is active (check logs)
oc logs -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator --tail=20 | \
  grep -i "Reconciling UpgradePreflightCheck"
```

---

## Test HTTP Service

### Setup Port Forward

```bash
# Get authentication token
export TOKEN=$(oc whoami -t)

# Start port-forward to HTTP service
oc port-forward -n openshift-managed-upgrade-operator \
  svc/managed-upgrade-operator-preflight 8443:8443 &

# Save PID for cleanup
export PF_PID=$!

# Wait for port-forward to be ready
sleep 3
```

### Test 1: Health Check

```bash
curl -k -X GET https://localhost:8443/healthz
# Expected: 200 OK (may be empty body)
```

### Test 2: Create Preflight Check

```bash
# Create a preflight check
RESPONSE=$(curl -k -s -X POST https://localhost:8443/api/v1/preflight-checks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "targetVersion": "4.15.0",
    "targetChannel": "stable-4.15",
    "platform": "OSD",
    "description": "Test preflight check"
  }')

echo "$RESPONSE" | jq '.'

# Expected output:
# {
#   "requestId": "uuid-here",
#   "status": "accepted",
#   "statusUrl": "/api/v1/preflight-checks/{uuid}",
#   "message": "Preflight check accepted"
# }

# Save request ID for later tests
export REQUEST_ID=$(echo "$RESPONSE" | jq -r '.requestId')
echo "Request ID: $REQUEST_ID"
```

### Test 3: Get Check Status

```bash
# Wait for checks to run
sleep 5

# Get check status
curl -k -s -X GET "https://localhost:8443/api/v1/preflight-checks/$REQUEST_ID" \
  -H "Authorization: Bearer $TOKEN" | jq '.'

# Expected fields:
# - requestId
# - status: "running", "completed", or "pending"
# - targetVersion, targetChannel, platform
# - checksTotal, checksCompleted, checksPassed, checksFailed
```

### Test 4: List All Checks

```bash
curl -k -s -X GET https://localhost:8443/api/v1/preflight-checks \
  -H "Authorization: Bearer $TOKEN" | jq '.'

# Expected output:
# {
#   "requests": [ array of check objects ],
#   "total": <count>
# }
```

### Test 5: Test Limit Enforcement

```bash
# Create checks until limit is reached
for i in {1..12}; do
  echo "Creating check $i..."
  RESP=$(curl -k -s -w "\nHTTP:%{http_code}" \
    -X POST https://localhost:8443/api/v1/preflight-checks \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{
      \"targetVersion\": \"4.15.$i\",
      \"targetChannel\": \"stable-4.15\",
      \"platform\": \"OSD\",
      \"description\": \"Check $i\"
    }")

  HTTP_CODE=$(echo "$RESP" | grep "HTTP:" | cut -d: -f2)
  echo "Status: $HTTP_CODE"

  if [ "$HTTP_CODE" = "429" ]; then
    echo "✅ Limit reached at check $i"
    echo "Error message:"
    echo "$RESP" | grep -v "HTTP:"
    break
  fi

  sleep 1
done
```

**Expected:** 10 checks succeed (202), 11th check gets 429 with helpful message

### Test 6: Delete Check

```bash
# Delete the first check we created
curl -k -s -w "\nHTTP:%{http_code}" \
  -X DELETE "https://localhost:8443/api/v1/preflight-checks/$REQUEST_ID" \
  -H "Authorization: Bearer $TOKEN"

# Expected: HTTP:204
# No body (204 No Content)
```

### Test 7: Verify Delete Worked

```bash
# Try to get the deleted check
curl -k -s -w "\nHTTP:%{http_code}" \
  -X GET "https://localhost:8443/api/v1/preflight-checks/$REQUEST_ID" \
  -H "Authorization: Bearer $TOKEN"

# Expected: HTTP:404
```

### Cleanup Port Forward

```bash
# Kill port-forward process
kill $PF_PID 2>/dev/null
wait $PF_PID 2>/dev/null
```

---

## Verify Preflight Checks

### Check CRs in Cluster

```bash
# List all UpgradePreflightCheck CRs
oc get upgradepreflightchecks -n openshift-managed-upgrade-operator

# View details of a specific check
CR_NAME=$(oc get upgradepreflightchecks \
  -n openshift-managed-upgrade-operator \
  -l "preflight.managed.openshift.io/request-id=$REQUEST_ID" \
  -o name | head -1)

oc get $CR_NAME -n openshift-managed-upgrade-operator -o yaml
```

### Expected CR Status Structure

```yaml
status:
  checks:
  - execution:
      state: Completed    # or Error, Skipped
    name: ClusterVersion
    result:
      details:
      - currentVersion=4.XX.XX
      verdict: Passed      # or Failed
  - execution:
      state: Completed
    name: CriticalAlerts
    result:
      verdict: Passed
  - name: ClusterOperators
    # ... (9 checks total)
  completedAt: "2026-11-02T23:45:00Z"
  startedAt: "2026-11-02T23:44:59Z"
```

### The 9 Preflight Checks

**Original 6 checks:**
1. **ClusterVersion** - Verifies cluster version
2. **CriticalAlerts** - Checks for blocking critical alerts
3. **ClusterOperators** - Ensures all cluster operators are healthy
4. **ManuallyCordonedNodes** - Detects manually cordoned nodes
5. **NodeUnschedulableTaints** - Checks for unschedulable node taints
6. **PodDisruptionBudgets** - Validates PDBs won't block upgrade

**New checks (from PR #2):**
7. **UpgradeableCondition** - Checks CVO Upgradeable=False condition, surfaces reasons (AdminAckRequired, incompatible operators)
8. **PullSecretValid** - Validates pull-secret has required cloud.openshift.com auth entry
9. **AdminAcks** - Reads admin-acks ConfigMap, reports if missing when target version specified

### Check TTL Annotations

```bash
# Verify 24-hour TTL is set
oc get upgradepreflightchecks -n openshift-managed-upgrade-operator \
  -o yaml | grep "delete-after"

# Should show timestamps 24 hours in the future
```

---

## Troubleshooting

### Operator Pod Not Starting

```bash
# Check pod events
oc describe pod -n openshift-managed-upgrade-operator \
  -l name=managed-upgrade-operator

# Check image pull
oc get events -n openshift-managed-upgrade-operator --sort-by='.lastTimestamp'

# Common issues:
# - Image not found: Verify $OPERATOR_IMAGE is pushed and public
# - ImagePullBackOff: Check registry authentication
# - CrashLoopBackOff: Check logs for startup errors
```

### HTTP Service Not Responding

```bash
# Verify service exists
oc get svc -n openshift-managed-upgrade-operator \
  managed-upgrade-operator-preflight

# Check if pod is listening on 8443
oc exec -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator -- \
  netstat -ln | grep 8443

# Check TLS certificate
oc get secret -n openshift-managed-upgrade-operator muo-preflight-tls

# View service logs
oc logs -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator | \
  grep "preflight-http"
```

### Checks Not Running (Status: pending)

```bash
# 1. Verify feature gate is enabled
oc get configmap -n openshift-managed-upgrade-operator \
  managed-upgrade-operator-config -o yaml | \
  grep -A 10 "featureGate:"

# Should include "PreflightController"

# 2. Check operator logs for feature gate warning
oc logs -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator | \
  grep "PreflightController feature gate"

# 3. Restart operator to pick up config
oc rollout restart deployment/managed-upgrade-operator \
  -n openshift-managed-upgrade-operator

# 4. Check controller reconciliation
oc logs -n openshift-managed-upgrade-operator \
  deployment/managed-upgrade-operator --tail=50 | \
  grep "Reconciling UpgradePreflightCheck"
```

### 403 Forbidden Errors

```bash
# Verify RBAC permissions
oc get clusterrole managed-upgrade-operator -o yaml | \
  grep -A 10 "upgradepreflightchecks"

# Should show:
# - create, delete, get, list, patch, update, watch
# - tokenreviews: create
# - subjectaccessreviews: create

# Check service account token
TOKEN=$(oc whoami -t)
echo ${#TOKEN}  # Should be 50+ characters
```

### Webhook Blocking CR Creation

This is **expected behavior**! The webhook `sre-regular-user-validation` blocks direct CR creation. This is why the HTTP service exists - it creates CRs using the operator's service account which bypasses the webhook.

```bash
# This should FAIL with webhook error:
oc apply -f - <<EOF
apiVersion: upgrade.managed.openshift.io/v1alpha1
kind: UpgradePreflightCheck
metadata:
  name: test
  namespace: openshift-managed-upgrade-operator
spec:
  type: OSD
  desired:
    version: "4.15.0"
    channel: "stable-4.15"
  upgradeAt: "2026-01-01T00:00:00Z"
  PDBForceDrainTimeout: 60
EOF

# Expected error:
# Error: admission webhook "regular-user-validation.managed.openshift.io" denied the request

# Use HTTP service instead (this works):
curl -k -X POST https://localhost:8443/api/v1/preflight-checks ...
```

### Architecture Mismatch Errors

```bash
# If you see: "exec format error" or "cannot execute binary file"
# The image was built for wrong architecture

# Verify image architecture
podman inspect $OPERATOR_IMAGE | jq '.[0].Architecture'
# Must be "amd64" for OpenShift

# Rebuild with correct platform:
podman build --platform=linux/amd64 -f build/Dockerfile -t $OPERATOR_IMAGE .
```

---

## Expected Timeline

| Phase | Duration | Notes |
|-------|----------|-------|
| Checkout code | 1-2 min | Depends on network speed |
| Build operator image | 5-10 min | macOS with QEMU, 2-3 min on Linux |
| Build PKO image | < 30 sec | Simple COPY operation |
| Push images | 2-5 min | Depends on network speed |
| Deploy to cluster | 1-2 min | PKO unpack + pod startup |
| Enable feature gate | 1 min | ConfigMap edit + rollout restart |
| Test HTTP service | 5 min | All CRUD operations |
| **Total** | **15-25 min** | End-to-end |

---

## Quick Reference: Common Commands

```bash
# Build and push (one-liner)
export TAG="pr-1-v$(date +%s)"
podman build --platform=linux/amd64 -f build/Dockerfile \
  -t quay.io/YOUR_USERNAME/managed-upgrade-operator:$TAG . && \
podman push quay.io/YOUR_USERNAME/managed-upgrade-operator:$TAG

# Deploy (one-liner)
cat <<EOF | oc apply -f -
apiVersion: package-operator.run/v1alpha1
kind: ClusterPackage
metadata:
  name: managed-upgrade-operator
spec:
  image: quay.io/YOUR_USERNAME/managed-upgrade-operator-pko:pr-1-test
  config:
    image: quay.io/YOUR_USERNAME/managed-upgrade-operator:$TAG
EOF

# Test (one-liner)
TOKEN=$(oc whoami -t) && \
oc port-forward -n openshift-managed-upgrade-operator \
  svc/managed-upgrade-operator-preflight 8443:8443 &
sleep 3 && \
curl -k -X POST https://localhost:8443/api/v1/preflight-checks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"targetVersion":"4.15.0","targetChannel":"stable-4.15","platform":"OSD"}'
```

---

## Success Criteria

### ✅ Deployment Successful
- [ ] ClusterPackage status: AVAILABLE=True
- [ ] Operator pod: READY=1/1, STATUS=Running
- [ ] Image architecture: amd64
- [ ] HTTP service: Listening on :8443
- [ ] TLS certificate: Present and valid
- [ ] Feature gate: PreflightController enabled

### ✅ HTTP Service Working
- [ ] POST /api/v1/preflight-checks → 202 Accepted
- [ ] GET /api/v1/preflight-checks/{id} → 200 OK with status
- [ ] GET /api/v1/preflight-checks → 200 OK with list
- [ ] DELETE /api/v1/preflight-checks/{id} → 204 No Content
- [ ] Limit enforcement → 429 Too Many Requests at 11th check
- [ ] Helpful error messages on limit/auth failures

### ✅ Preflight Checks Running
- [ ] CR status updates within 5 seconds
- [ ] All 9 checks execute: ClusterVersion, CriticalAlerts, ClusterOperators, ManuallyCordonedNodes, NodeUnschedulableTaints, PodDisruptionBudgets, UpgradeableCondition, PullSecretValid, AdminAcks
- [ ] Verdict: Passed or Failed (with details)
- [ ] Execution state: Completed (or Error/Skipped where appropriate)
- [ ] Timestamps: startedAt and completedAt populated

### ✅ Resource Management
- [ ] TTL annotation: delete-after set to +24 hours
- [ ] Auto-cleanup: Expired CRs deleted on next POST
- [ ] DELETE endpoint: Removes CR immediately
- [ ] Limit enforcement: Prevents 11th+ CR creation

---

## Additional Resources

- **PR #1 (original)**: https://github.com/nephomaniac/managed-upgrade-operator/pull/1
- **Our fixes branch**: https://github.com/nephomaniac/managed-upgrade-operator/tree/maclark/fix-preflight-http-service
- **Design docs**: See `/sandbox/memory/project_preflight_http_service.md`
- **Session notes**: See memory files in `.claude/projects/.../memory/`

---

## Getting Help

If you encounter issues not covered in troubleshooting:

1. **Check operator logs**: Most issues are logged with clear error messages
2. **Verify prerequisites**: Ensure all tools are correct versions
3. **Check RBAC**: Many permission errors are RBAC-related
4. **Review git commits**: Ensure you're on the correct branch with all fixes
5. **Architecture**: macOS users must use `--platform=linux/amd64`

**Common mistakes:**
- Building without `--platform=linux/amd64` on macOS → binary won't run
- Forgetting to enable PreflightController feature gate → checks stay pending
- Using wrong image tags → cluster pulls old/wrong image
- Not restarting operator after ConfigMap change → new config not loaded
