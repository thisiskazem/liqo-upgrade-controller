# Liqo Upgrade Controller - Phase 1 Complete ✅

## Overview

**Production-grade CRD upgrade** with backup, rollback, and health checks:

- ✅ **Automatic Backup** - Saves all CRDs before upgrade
- ✅ **Version Validation** - Prevents wrong version upgrades
- ✅ **Smart Comparison** - Only upgrades changed CRDs (SHA256 hash)
- ✅ **Health Verification** - Validates cluster health after upgrade
- ✅ **Automatic Rollback** - Restores backup on failure
- ✅ **TTL Cleanup** - Jobs auto-delete after 5 minutes

---

## Architecture

### Phase Flow

```
User applies CR → Backup → Validate → Upgrade → Verify → Complete
                    ↓                     ↓
                  Saved              If fails → Rollback
```

### Jobs Created

1. **liqo-backup-*** - Backs up 30 Liqo CRDs
2. **liqo-upgrade-crd-*** - Compares and applies changes
3. **liqo-verify-crd-*** - Health checks
4. **liqo-rollback-crd-*** (only on failure) - Restores backup

All jobs have TTL=300s (auto-delete 5 minutes after completion)

---

## Deployment

### Prerequisites

- Liqo v1.0.0 installed
- kubectl access to cluster
- Docker Hub account

### Step 1: Setup Files

```bash
cd liqo-upgrade-controller

# Copy all required files
cp liqoupgrade_types.go api/v1alpha1/
cp liqoupgradebackup_types.go api/v1alpha1/
cp liqoupgrade_controller_fixed.go internal/controller/liqoupgrade_controller.go
cp kustomization.yaml config/crd/
```

### Step 2: Build & Deploy

```bash
# Generate code
make manifests generate

# Build image
make docker-build IMG=kazem26/liqo-upgrade-controller:v0.2.1

# Push image
make docker-push IMG=kazem26/liqo-upgrade-controller:v0.2.1

# Deploy to cluster
make install
make deploy IMG=kazem26/liqo-upgrade-controller:v0.2.1

# Apply RBAC
kubectl apply -f upgrade-rbac.yaml
```

### Step 3: Test

```bash
# Apply upgrade
kubectl apply -f test-upgrade.yaml

# Watch controller
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f

# Check status
kubectl get liqoupgrade -n liqo -w
```

---

## Upgrade Process

### 1. Backup Phase (30 seconds)

```bash
$ kubectl logs -n liqo liqo-backup-*

=========================================
Backing up Liqo CRDs
=========================================
Found 30 Liqo CRDs to backup
Backing up: configurations.networking.liqo.io
Backing up: foreignclusters.core.liqo.io
...
✅ Backup completed successfully!
Backed up 30 CRDs
```

### 2. Version Validation

```bash
Step 0: Validating current version...
  User specified: v1.0.0
  Cluster has: v1.0.0
  ✅ Version validation passed!
```

### 3. Smart Comparison

```bash
Step 2: Comparing CRDs between versions...
  Checking: ips.ipam.liqo.io
    → CHANGED (adding to upgrade list)
  Checking: configurations.networking.liqo.io
    → No changes
...
Comparison Summary:
  - Changed CRDs: 6
  - New CRDs: 0
  - Removed CRDs: 0
```

### 4. Apply Changes

```bash
Step 3: Applying changed/new CRDs...
  Applying: ips.ipam.liqo.io
    ✓ Applied successfully
  Applying: wggatewayclients.networking.liqo.io
    → Annotation too large, using server-side apply
✅ CRD upgrade completed successfully!
```

### 5. Health Verification

```bash
$ kubectl logs -n liqo liqo-verify-crd-*

Step 1: Checking all Liqo CRDs exist...
  Found 30 Liqo CRDs
  ✅ CRD count looks good

Step 2: Verifying CRDs are valid...
  ✅ All CRDs are valid

Step 4: Verifying Liqo components...
  ✅ liqo-controller-manager is running

✅ Verification Passed!
```

### 6. Final Status

```yaml
status:
  phase: Completed
  message: "Phase 1 (CRD upgrade) completed successfully"
  backupReady: true
  backupName: "liqo-backup-liqo-upgrade-test"
  lastSuccessfulPhase: UpgradingCRDs
  rolledBack: false
  lastUpdated: "2025-10-30T..."
```

---

## Rollback on Failure

If any phase fails, automatic rollback:

```bash
Controller logs:
  INFO  CRD upgrade failed, initiating rollback
  INFO  Starting rollback  reason="CRD upgrade job failed"
  INFO  Rollback job created successfully

Rollback job:
  =========================================
  Rolling Back CRDs
  =========================================
  Found backup with 30 CRDs
  Restoring CRDs from backup...
    Restoring: configurations.networking.liqo.io
    Restoring: foreignclusters.core.liqo.io
    ...
  ✅ Rollback completed successfully!

Final status:
  phase: Failed
  message: "Upgrade failed and rolled back successfully"
  rolledBack: true
```

---

## Key Features Explained

### 1. Version Validation

Reads actual Liqo version from `liqo-controller-manager` deployment:

```bash
CONTROLLER_IMAGE=$(kubectl get deployment liqo-controller-manager -n liqo \
  -o jsonpath='{.spec.template.spec.containers[0].image}')
ACTUAL_VERSION="${CONTROLLER_IMAGE##*:}"  # Extract version from image tag
```

Fails immediately if mismatch detected.

### 2. Smart CRD Comparison

```bash
# Downloads CRDs for both versions from GitHub
curl https://raw.githubusercontent.com/liqotech/liqo/v1.0.0/...
curl https://raw.githubusercontent.com/liqotech/liqo/v1.0.1/...

# Compares SHA256 hashes
sha256sum current.yaml → abc123...
sha256sum target.yaml  → def456...

# Only applies if hashes differ
```

### 3. Server-Side Apply for Large CRDs

Automatically detects when `kubectl apply` fails due to annotation size:

```bash
if kubectl apply fails with "Too long":
  kubectl apply --server-side --force-conflicts
```

### 4. TTL-Based Cleanup

All jobs have `ttlSecondsAfterFinished: 300`:

- Jobs complete → Wait 5 minutes → Kubernetes auto-deletes
- Keeps cluster clean
- Logs available for 5 minutes for debugging

---

## Troubleshooting

### Controller Not Starting

```bash
kubectl get pods -n liqo-upgrade-controller-system
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager
```

### Jobs Not Creating

```bash
# Check RBAC
kubectl get sa liqo-upgrade-controller -n liqo
kubectl get clusterrole liqo-upgrade-job-role
kubectl get clusterrolebinding liqo-upgrade-job-binding

# Apply if missing
kubectl apply -f upgrade-rbac.yaml
```

### Version Mismatch Error

```bash
❌ ERROR: Version mismatch detected!
  You specified currentVersion: v1.0.0
  But cluster actually has: v0.9.5

# Fix: Update test-upgrade.yaml with correct version
spec:
  currentVersion: v0.9.5  # Match cluster
```

### Clean Retry

```bash
kubectl delete liqoupgrade --all -n liqo
kubectl delete jobs -n liqo --all
kubectl apply -f test-upgrade.yaml
```

---

## Verification Commands

```bash
# Check upgrade status
kubectl get liqoupgrade -n liqo

# Get detailed status
kubectl get liqoupgrade liqo-upgrade-test -n liqo -o yaml | grep -A 10 status

# Check all jobs
kubectl get jobs -n liqo

# View job logs
kubectl logs -n liqo liqo-backup-*
kubectl logs -n liqo liqo-upgrade-crd-*
kubectl logs -n liqo liqo-verify-crd-*

# Verify Liqo still works
kubectl get pods -n liqo
kubectl get foreignclusters -A
```

---

## Next Phases

- **Phase 2**: Control Plane upgrade (controller-manager, webhook, proxy)
- **Phase 3**: Data Plane upgrade (virtual-kubelet, gateway, network fabric)
- **Phase 4**: Multi-cluster coordination
- **Phase 5**: Advanced rollback strategies

---

## Files Included

1. **liqoupgrade_types.go** - API types with backup/rollback fields
2. **liqoupgradebackup_types.go** - Backup CRD types
3. **liqoupgrade_controller_fixed.go** - Controller with all fixes
4. **kustomization.yaml** - CRD kustomization
5. **upgrade-rbac.yaml** - ServiceAccount + permissions
6. **test-upgrade.yaml** - Test upgrade manifest

---

## Production Readiness

✅ **Tested** - Phase 1 works end-to-end  
✅ **Safe** - Backup before, rollback on failure  
✅ **Observable** - Clear logs and status updates  
✅ **Clean** - TTL auto-cleanup  
✅ **Validated** - Version checking prevents mistakes  

**Phase 1 is production-ready for CRD upgrades.**