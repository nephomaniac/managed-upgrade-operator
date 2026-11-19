# Task for Remote Agent: Preserve Existing Features in Your Branch

## Problem

Your commits `d76f294` and `6e6b862` removed several features that need to be preserved. Your UI improvements are excellent, but we need to merge them with the existing Cincinnati API integration.

## What You Need to Do

### Step 1: Sync Your Branch With nephomaniac Fork

```bash
# Add nephomaniac as a remote if you haven't already
git remote add nephomaniac https://github.com/nephomaniac/managed-upgrade-operator.git

# Fetch latest from nephomaniac
git fetch nephomaniac

# Check what commits you're missing
git log rosa-agent/maclark/check-pattern-refactor..nephomaniac/maclark/check-pattern-refactor --oneline

# You should see these commits:
# f8893cd feat: merge structured findings implementation with Cincinnati API
# a74df63 docs: add structured findings proposal for upgrade diagnostics
# aa4b558 refactor: move display text from console plugin to backend API
# 1b238e1 feat: add dynamic version fetching with Cincinnati API
# 3e6911a docs: sanitize personal and system-specific references
```

### Step 2: Cherry-Pick Your UI Improvements on Top of Our Work

```bash
# Start from nephomaniac's latest
git checkout maclark/check-pattern-refactor
git reset --hard nephomaniac/maclark/check-pattern-refactor

# Now cherry-pick YOUR two commits
git cherry-pick d76f294  # Phase C stepper
git cherry-pick 6e6b862  # Polish + structured findings rendering

# If there are conflicts, resolve them while KEEPING these files:
# - controllers/upgradediags/available_versions.go
# - controllers/upgradediags/http_service.go  
# - consoleplugin/src/components/CreateDiagsCheckForm.tsx (Cincinnati integration)
# - PROPOSAL_STRUCTURED_FINDINGS.md
# - api/v1alpha1/upgradediagscheck_types.go (ownerLabel field)
# - controllers/upgradediags/check_helpers.go (withOwner auto-population)
```

### Step 3: Resolve Any Conflicts

If `git cherry-pick` reports conflicts, resolve them like this:

**For `consoleplugin/src/components/UpgradeDiagsDetail.tsx`:**
- Keep your `StructuredFindingsList` component (lines ~111-177)
- Keep the structured findings rendering (lines ~244-257)
- Merge both versions

**For other files:**
- If a file was removed by your commit but exists in nephomaniac fork, KEEP IT
- Example: `available_versions.go`, `http_service.go` should NOT be deleted

### Step 4: Verify All Required Features Are Present

Before pushing, run these checks:

```bash
# 1. Cincinnati API backend files exist
test -f controllers/upgradediags/available_versions.go && echo "✅ Cincinnati API present" || echo "❌ MISSING"
test -f controllers/upgradediags/http_service.go && echo "✅ HTTP routes present" || echo "❌ MISSING"

# 2. Proposal doc exists
test -f PROPOSAL_STRUCTURED_FINDINGS.md && echo "✅ Proposal doc present" || echo "❌ MISSING"

# 3. ownerLabel auto-population in check_helpers.go
grep -q "Admin action required" controllers/upgradediags/check_helpers.go && echo "✅ ownerLabel auto-set" || echo "❌ MISSING"

# 4. ownerLabel field in API types
grep -q "OwnerLabel string" api/v1alpha1/upgradediagscheck_types.go && echo "✅ ownerLabel field" || echo "❌ MISSING"

# 5. Cincinnati integration in CreateDiagsCheckForm
grep -q "fetchAvailableVersions" consoleplugin/src/components/CreateDiagsCheckForm.tsx && echo "✅ Cincinnati UI" || echo "❌ MISSING"

# ALL checks must pass before pushing
```

### Step 5: Push Your Updated Branch

```bash
# Only push if ALL verification checks passed
git push -f rosa-agent maclark/check-pattern-refactor
```

## Why This Workflow?

**Current situation:**
- nephomaniac fork: Has Cincinnati API + Structured Findings + UI base
- rosa-agent fork: Has UI improvements but missing Cincinnati API

**Goal:**
- Merge BOTH sets of features into one branch
- rosa-agent branch should include EVERYTHING

**Method:**
- Cherry-pick = Take your commits and apply them on top of our work
- This preserves both Cincinnati API AND your UI improvements

## Features That MUST Be Present

| Feature | Files | Purpose |
|---------|-------|---------|
| **Cincinnati API** | `available_versions.go`, `http_service.go` | Backend endpoint for dynamic version fetching |
| **Cincinnati UI** | `CreateDiagsCheckForm.tsx` (lines ~170-250) | Fetches versions from API when channel changes |
| **ownerLabel Auto-Set** | `check_helpers.go` `withOwner()` function | Auto-populates "Admin action required" |
| **ownerLabel Field** | `upgradediagscheck_types.go` | CRD field for backend-driven labels |
| **Proposal Doc** | `PROPOSAL_STRUCTURED_FINDINGS.md` | Documents structured findings design |
| **Your UI Stepper** | `UpgradeStatusView.tsx` UpgradePhaseStepper | 5-station progress view |
| **Your Findings List** | `UpgradeDiagsDetail.tsx` StructuredFindingsList | Renders structured findings |

## Expected Final State

After completing these steps, your branch should have:

✅ Cincinnati API backend (`available_versions.go`, `http_service.go`)
✅ Cincinnati UI integration (`CreateDiagsCheckForm.tsx` with dynamic version fetching)
✅ ownerLabel auto-population (`check_helpers.go`)
✅ Proposal documentation (`PROPOSAL_STRUCTURED_FINDINGS.md`)
✅ UpgradePhaseStepper component (your work)
✅ StructuredFindingsList component (your work)
✅ 30s auto-refresh (your work)
✅ Auto-open active station (your work)

## Alternative: Patch File Method

If cherry-pick is complicated, you can also apply your changes as patches:

```bash
# Generate patches for your two commits
git format-patch -2 6e6b862

# Start from nephomaniac's branch
git checkout maclark/check-pattern-refactor  
git reset --hard nephomaniac/maclark/check-pattern-refactor

# Apply your patches
git am 0001-*.patch
git am 0002-*.patch

# Resolve conflicts, verify features, push
```

## Questions?

If you're unsure about any step or encounter conflicts you can't resolve:
1. Push your branch as-is with a note
2. Describe what conflicts you encountered
3. We'll provide specific merge guidance

The key point: **Don't start from a stale branch next time. Always fetch and merge nephomaniac fork first.**

---

## Summary

**DO:**
1. `git fetch nephomaniac`
2. `git reset --hard nephomaniac/maclark/check-pattern-refactor`
3. `git cherry-pick d76f294 6e6b862`
4. Verify all features present
5. Push

**Result:**
- Your UI improvements + Our Cincinnati API = Complete feature set
- No features lost, everything works together

Please implement this and push the result. Thanks!
