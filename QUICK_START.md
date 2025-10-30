# Quick Start Guide - Liqo Smart Upgrade Controller

## Phase 1: Complete with Backup, Rollback & Health Checks ✅

### What You Get

✅ **Automatic Backup** before every upgrade  
✅ **Smart CRD Comparison** (only upgrades changed CRDs)  
✅ **Version Validation** (prevents wrong version upgrades)  
✅ **Health Verification** after upgrade  
✅ **Automatic Rollback** on failure  
✅ **TTL Cleanup** (jobs auto-delete after 5 minutes)

---

## Required Files

### 1. **liqoupgrade_types.go**
   - **Where**: `api/v1alpha1/liqoupgrade_types.go`
   - Enhanced with backup/rollback status fields

### 2. **liqoupgradebackup_types.go**
   - **Where**: `api/v1alpha1/liqoupgradebackup_types.go`
   - New CRD for backup state

### 3. **liqoupgrade_controller_fixed.go**
   - **Where**: `internal/controller/liqoupgrade_controller.go`
   - Complete controller with all fixes

### 4. **kustomization.yaml**
   - **Where**: `config/crd/kustomization.yaml`
   - Includes both CRDs

### 5. **upgrade-rbac.yaml**
   - **Where**: Apply to cluster
   - Includes deployment read permission

### 6. **test-upgrade.yaml**
   - **Where**: Apply to cluster
   - Clean CR without status

---

## Quick Deploy

```bash
# 1. Copy files
cp liqoupgrade_types.go api/v1alpha1/
cp liqoupgradebackup_types.go api/v1alpha1/
cp liqoupgrade_controller_fixed.go internal/controller/liqoupgrade_controller.go
cp kustomization.yaml config/crd/

# 2. Generate and build
make manifests generate
make docker-build docker-push IMG=kazem26/liqo-upgrade-controller:v0.2.1

# 3. Deploy
make install
make deploy IMG=kazem26/liqo-upgrade-controller:v0.2.1
kubectl apply -f upgrade-rbac.yaml

# 4. Test
kubectl apply -f test-upgrade.yaml

# 5. Watch
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f
```

---

## Complete Flow

```
1. Backup Phase (30s)
   └─> Backs up all 30 Liqo CRDs

2. Version Validation
   └─> Confirms v1.0.0 matches cluster

3. CRD Upgrade (1-2min)
   └─> Compares & applies 6 changed CRDs
   └─> Uses server-side apply for large CRDs

4. Verification (30s)
   └─> Checks CRDs valid
   └─> Confirms liqo-controller-manager running

5. Completion
   └─> Status: Completed
   └─> Jobs auto-delete after 5 minutes

If failure → Automatic rollback to backup
```

---

## Expected Logs

```
INFO  Reconciling upgrade  currentPhase=""
INFO  Phase is empty, starting backup
INFO  Starting backup phase  version="v1.0.0"
INFO  Backup completed successfully
INFO  Starting CRD upgrade phase
INFO  CRD upgrade completed, starting verification
INFO  Verification passed, Phase 1 complete!
INFO  Upgrade completed successfully
```

---

## Verify Success

```bash
# Check status
kubectl get liqoupgrade liqo-upgrade-test -n liqo

# Expected output:
NAME                CURRENT   TARGET   PHASE       AGE
liqo-upgrade-test   v1.0.0    v1.0.1   Completed   3m

# Check details
kubectl get liqoupgrade liqo-upgrade-test -n liqo -o yaml

# Expected status:
status:
  phase: Completed
  message: "Phase 1 (CRD upgrade) completed successfully"
  backupReady: true
  backupName: "liqo-backup-liqo-upgrade-test"
  lastSuccessfulPhase: UpgradingCRDs
  rolledBack: false
```

---

## What Changed from v0.1

### Fixed:
1. **CRD Apply Logic** - Now handles large CRDs correctly with server-side apply
2. **Rollback System** - Automatic backup and rollback on failure
3. **State Machine** - Clear logging of phase transitions
4. **Job Cleanup** - TTL auto-deletes jobs after 5 minutes

### New Features:
- Pre-upgrade backup
- Post-upgrade health checks
- Automatic rollback capability
- Enhanced observability

---

## Troubleshooting

### Jobs Don't Start
```bash
kubectl describe job -n liqo
# Fix: Check RBAC applied
kubectl apply -f upgrade-rbac.yaml
```

### Version Mismatch
```bash
kubectl logs -n liqo -l job-name=liqo-upgrade-crd-*
# Fix: Update currentVersion in test-upgrade.yaml
```

### Clean Retry
```bash
kubectl delete liqoupgrade --all -n liqo
kubectl delete jobs -n liqo --all
kubectl apply -f test-upgrade.yaml
```