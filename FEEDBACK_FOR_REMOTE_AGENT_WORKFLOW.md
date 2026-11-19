# CRITICAL: Remote Agent Workflow Issue - Features Being Removed

## Problem

Your commits are removing features we've already implemented because you're branching from stale code. This has happened twice now:

### Features Removed (But Need to be Preserved):

1. **Cincinnati API Integration** (`available_versions.go`, `http_service.go`)
   - Dynamic version fetching from Cincinnati API
   - `/api/v1/available-versions` endpoint
   - Server-side caching (1 hour TTL)
   - **Status**: MUST KEEP - this is a shipped feature

2. **CreateDiagsCheckForm.tsx Cincinnati Integration**
   - Console plugin fetches versions dynamically when channel changes
   - Manual refresh capability
   - Cache status display
   - **Status**: MUST KEEP - actively used in UI

3. **PROPOSAL_STRUCTURED_FINDINGS.md**
   - Design document for structured findings
   - **Status**: KEEP - important documentation

4. **ownerLabel Auto-Population in check_helpers.go**
   - `withOwner()` function sets ownerLabel = "Admin action required" / "Red Hat action required"
   - **Status**: MUST KEEP - this is the backend implementation of your structured findings

5. **api/v1alpha1/upgradediagscheck_types.go ownerLabel field**
   - `OwnerLabel string` field in DiagsCheckResult
   - **Status**: MUST KEEP - part of CRD schema

## Root Cause

You're branching from an old commit in YOUR fork (rosa-agent) instead of pulling the latest from OUR fork (nephomaniac).

**Current broken workflow:**
```bash
# ❌ WRONG - You're doing this:
git checkout maclark/check-pattern-refactor  # Uses stale rosa-agent version
# ... make changes ...
git commit
git push rosa-agent maclark/check-pattern-refactor
```

**This causes you to:**
1. Start with code from commit `12b89f7` (old)
2. Miss commits `3e6911a`, `1b238e1`, `aa4b558`, `a74df63` (our work)
3. When you push, you effectively "revert" our features

## Required Workflow Fix

**ALWAYS sync with nephomaniac fork BEFORE starting new work:**

```bash
# Step 1: Fetch latest from BOTH forks
git fetch nephomaniac
git fetch rosa-agent

# Step 2: Check what's on nephomaniac fork
git log rosa-agent/maclark/check-pattern-refactor..nephomaniac/maclark/check-pattern-refactor --oneline

# Step 3: If nephomaniac has commits you don't have, MERGE FIRST
git checkout maclark/check-pattern-refactor
git merge nephomaniac/maclark/check-pattern-refactor

# Step 4: Now make your changes
# ... your work ...

# Step 5: Commit and push
git commit -m "..."
git push rosa-agent maclark/check-pattern-refactor
```

## Why This Happens

**Git branching model issue:**
```
nephomaniac/maclark/check-pattern-refactor:  A -- B -- C -- D (Cincinnati, ownerLabel, etc.)
                                                       |
rosa-agent/maclark/check-pattern-refactor:   A -- B -- X (your work, missing C & D)
                                                       |
                                            When you push: reverts C & D
```

**What should happen:**
```
nephomaniac/maclark/check-pattern-refactor:  A -- B -- C -- D
                                                            |
rosa-agent/maclark/check-pattern-refactor:   A -- B -- C -- D -- X (your work, includes C & D)
                                                                  |
                                                       All features preserved
```

## Features Matrix - What MUST Be Present

| Feature | File | Why It's Critical |
|---------|------|-------------------|
| Cincinnati API | `available_versions.go` | Backend endpoint for dynamic versions |
| Cincinnati Route | `http_service.go` | Routes `/api/v1/available-versions` |
| Version Fetching UI | `CreateDiagsCheckForm.tsx` | Fetches versions when channel changes |
| ownerLabel Auto-Set | `check_helpers.go` `withOwner()` | Auto-populates "Admin action required" |
| ownerLabel Field | `upgradediagscheck_types.go` | CRD field for backend-driven labels |
| Design Proposal | `PROPOSAL_STRUCTURED_FINDINGS.md` | Documents structured findings design |

**All of these must remain in EVERY commit you push.**

## How to Verify Before Pushing

**Before you push, run these checks:**

```bash
# 1. Verify Cincinnati files exist
ls -la controllers/upgradediags/available_versions.go
ls -la controllers/upgradediags/http_service.go

# 2. Verify ownerLabel auto-population
grep -A 10 "func withOwner" controllers/upgradediags/check_helpers.go | grep "Admin action required"

# 3. Verify proposal doc exists
ls -la PROPOSAL_STRUCTURED_FINDINGS.md

# 4. Verify CreateDiagsCheckForm has Cincinnati integration
grep -n "fetchAvailableVersions\|Cincinnati" consoleplugin/src/components/CreateDiagsCheckForm.tsx

# If ANY of these fail, you're missing our features - DO NOT PUSH
```

## Action Required

**For your NEXT commit:**

1. **BEFORE starting work:**
   ```bash
   git fetch nephomaniac
   git merge nephomaniac/maclark/check-pattern-refactor
   ```

2. **Verify you have our features:**
   ```bash
   git log --oneline | head -10
   # Should see commits:
   # - f8893cd feat: merge structured findings implementation with Cincinnati API
   # - a74df63 docs: add structured findings proposal
   # - aa4b558 refactor: move display text from console plugin to backend
   # - 1b238e1 feat: add dynamic version fetching with Cincinnati API
   ```

3. **Then make your changes on top of our work**

4. **Before pushing, verify all features present** (use checks above)

## Why We Keep Restoring Your Removed Features

When you remove our features, we have to:
1. Detect what was removed (git diff)
2. Restore the files with `git checkout HEAD -- <files>`
3. Merge your UI improvements separately
4. Push a merge commit that includes BOTH sets of work

This is time-consuming and error-prone. **Please sync FIRST to avoid this.**

## Summary

**DO THIS:**
✅ Always `git fetch nephomaniac` before starting work
✅ Always `git merge nephomaniac/maclark/check-pattern-refactor` to get latest
✅ Verify Cincinnati files exist before pushing
✅ Keep proposal documentation

**DON'T DO THIS:**
❌ Branch from stale rosa-agent code
❌ Remove files you didn't create
❌ Delete features without asking why they exist

## Questions?

If you're unsure whether a file should be removed, **ask first**:
- "Why does `available_versions.go` exist?"
- "What is `PROPOSAL_STRUCTURED_FINDINGS.md` for?"
- "Should I keep the Cincinnati integration in CreateDiagsCheckForm?"

We're happy to explain the purpose of any feature before you accidentally remove it.

---

**Current Status After Your Latest Push (6e6b862):**

Your UI improvements are excellent:
- ✅ UpgradePhaseStepper (5-station progress view)
- ✅ StructuredFindingsList (renders severity, owner, resource, action)
- ✅ Auto-refresh (30s with cleanup)
- ✅ Auto-open active/failed station

BUT you removed:
- ❌ Cincinnati API integration (must restore)
- ❌ ownerLabel auto-population (must restore)
- ❌ Proposal documentation (must restore)

We're merging your UI work while restoring the removed features. For next time, please sync first!
