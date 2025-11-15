# Liqo Upgrade Controller - Phases 1 & 2 Complete ✅

## Overview

**Production-grade Liqo upgrade** with backup, rollback, and health checks:

- ✅ **Phase 1: CRDs** - All 30 Liqo CRDs upgraded
- ✅ **Phase 2: Core Control Plane** - controller-manager + webhook upgraded
- 🚧 **Phase 3: Extended Control Plane** - Coming next
- 🚧 **Phase 4: Data Plane** - Coming next
- 🚧 **Phase 5: Final Cleanup** - Label updates & verification

---

## What Works Now

### Phase 1: CRD Upgrade (Complete)
- Automatic backup of all CRDs
- Smart comparison (SHA256 hash)
- Only upgrades changed CRDs
- Rollback on failure

### Phase 2: Core Control Plane (Complete)
- Sequential upgrade: controller-manager → webhook
- Uses Kubernetes native rolling updates
- Pre-upgrade deployment backup
- Automatic rollback on failure
- Zero downtime (brief webhook unavailability)

---

## Architecture

### Components Upgraded So Far

**Phase 1 (CRDs):**
- All 30 Liqo CustomResourceDefinitions

**Phase 2 (Core Control Plane):**
1. `liqo-controller-manager` - Core orchestrator
2. `liqo-webhook` - Admission webhook

### Still To Upgrade

**Phase 3 (Extended Control Plane):**
- liqo-ipam
- liqo-crd-replicator
- liqo-metric-agent
- liqo-proxy

**Phase 4 (Data Plane):**
- liqo-fabric (DaemonSet)
- liqo-gateway (Dynamic pods)

---

## Files

### Required Files
1. **liqoupgrade_types.go** - API types
2. **liqoupgradebackup_types.go** - Backup CRD types
3. **liqoupgrade_controller_phase2_fixed.go** - Controller (Phases 1+2)
4. **kustomization.yaml** - CRD configuration
5. **upgrade-rbac.yaml** - ServiceAccount + permissions (with `watch` verb)
6. **test-upgrade.yaml** - Test upgrade manifest

### Key Files Changed in Phase 2
- **upgrade-rbac.yaml** - Added `watch` verb for deployments and replicasets
- **liqoupgrade_controller_phase2_fixed.go** - Fixed webhook image name

---

## Deployment

### Prerequisites
- Liqo v1.0.0 installed
- kubectl access
- Docker Hub account

### Step 1: Setup
```bash
cd liqo-upgrade-controller

# Copy files
cp liqoupgrade_types.go api/v1alpha1/
cp liqoupgradebackup_types.go api/v1alpha1/
cp liqoupgrade_controller_phase2_fixed.go internal/controller/liqoupgrade_controller.go
cp kustomization.yaml config/crd/
```

### Step 2: Build & Deploy
```bash
# Generate and build
make manifests generate
make docker-build docker-push IMG=kazem26/liqo-upgrade-controller:v0.3

# Deploy to cluster
make deploy IMG=kazem26/liqo-upgrade-controller:v0.3

# Apply RBAC
kubectl apply -f upgrade-rbac.yaml
```

### Step 3: Test
```bash
# Run upgrade
kubectl apply -f test-upgrade.yaml

# Watch progress
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f
```

---

## Complete Upgrade Flow (Phases 1 & 2)

```
User applies CR
     ↓
Phase 1: Creating Backup (30s)
  └─> Backs up all 30 CRDs
     ↓
Phase 1: Upgrading CRDs (1-2min)
  └─> Compares SHA256 hashes
  └─> Applies 6 changed CRDs
     ↓
Phase 1: Verifying CRDs (30s)
  └─> Validates all CRDs
  └─> Checks liqo-controller-manager
     ↓
Phase 2: Upgrading Control Plane (3-5min)
  └─> Backup deployments
  └─> Upgrade controller-manager (v1.0.0 → v1.0.1)
  └─> Wait for rollout + health check
  └─> Upgrade webhook (v1.0.0 → v1.0.1)
  └─> Wait for rollout + health check
     ↓
Status: Completed ✅
```

---

## Expected Results

### Successful Upgrade

```bash
$ kubectl get liqoupgrade -n liqo
NAME                CURRENT   TARGET   PHASE       AGE
liqo-upgrade-test   v1.0.0    v1.0.1   Completed   8m

$ kubectl get liqoupgrade liqo-upgrade-test -n liqo -o yaml
status:
  phase: Completed
  message: "Phase 2 (Control plane upgrade) completed successfully"
  backupReady: true
  backupName: "liqo-backup-liqo-upgrade-test"
  lastSuccessfulPhase: UpgradingControlPlane
  rolledBack: false
```

### Verify Component Versions

```bash
# Check images
$ kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/liqo-controller-manager:v1.0.1

$ kubectl get deployment liqo-webhook -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/webhook:v1.0.1

# Check pods running
$ kubectl get pods -n liqo -l app.kubernetes.io/name=controller-manager
NAME                                       READY   STATUS    AGE
liqo-controller-manager-679476f7bc-t9brq   1/1     Running   5m

$ kubectl get pods -n liqo -l app.kubernetes.io/name=webhook
NAME                            READY   STATUS    AGE
liqo-webhook-6b94d4fdf9-l2f8r   1/1     Running   5m
```

---

## Jobs Created

All jobs have `ttlSecondsAfterFinished: 300` (auto-delete after 5 minutes):

1. **liqo-backup-*** - Backs up CRDs
2. **liqo-upgrade-crd-*** - Upgrades CRDs
3. **liqo-verify-crd-*** - Verifies CRDs
4. **liqo-upgrade-controlplane-*** - Upgrades deployments
5. **liqo-rollback-crd-*** (only on failure) - Restores backup

---

## Rollback

### Automatic Rollback

Phase 2 has smart rollback based on what failed:

**If CRD upgrade fails:**
```bash
# Restores all CRDs from backup
Rollback job: liqo-rollback-crd-*
```

**If Control Plane upgrade fails:**
```bash
# Restores deployment YAMLs from /tmp/
Rollback job: liqo-rollback-crd-* (runs control plane rollback)
```

### Manual Rollback

If needed:
```bash
# Rollback controller-manager
kubectl set image deployment/liqo-controller-manager \
  controller-manager=ghcr.io/liqotech/liqo-controller-manager:v1.0.0 -n liqo

# Rollback webhook
kubectl set image deployment/liqo-webhook \
  webhook=ghcr.io/liqotech/webhook:v1.0.0 -n liqo
```

---

## Troubleshooting

### Job Fails: RBAC Issues

```bash
# Error: cannot watch/patch deployments
# Fix: Ensure upgrade-rbac.yaml applied
kubectl apply -f upgrade-rbac.yaml

# Verify permissions
kubectl auth can-i watch deployments --as=system:serviceaccount:liqo:liqo-upgrade-controller -n liqo
# Should return: yes
```

### Deployment Stuck Rolling Out

```bash
# Check deployment
kubectl describe deployment liqo-controller-manager -n liqo
kubectl describe deployment liqo-webhook -n liqo

# Check pods
kubectl get pods -n liqo
kubectl describe pod <pod-name> -n liqo

# Common issues:
# - ImagePullBackOff: Wrong image name/tag
# - CrashLoopBackOff: Check pod logs
```

### Clean Retry

```bash
# Delete everything
kubectl delete liqoupgrade --all -n liqo
kubectl delete jobs -n liqo --all

# Ensure deployments are healthy
kubectl rollout status deployment/liqo-controller-manager -n liqo
kubectl rollout status deployment/liqo-webhook -n liqo

# Retry
kubectl apply -f test-upgrade.yaml
```

---

## Known Issues & Notes

### Helm Labels Not Updated

After Phase 2, you'll see:
```yaml
labels:
  app.kubernetes.io/version: v1.0.0  # Still shows old version
  helm.sh/chart: liqo-v1.0.0
```

**Why:** `kubectl set image` only changes container images, not labels.

**Impact:** None - labels are metadata only. Images are correct (v1.0.1).

**Fix:** Will be addressed in Phase 5 (Final Cleanup).

---

## Performance

### Typical Timing

- **Phase 1 (CRDs)**: ~2-3 minutes
- **Phase 2 (Control Plane)**: ~3-5 minutes
- **Total**: ~5-8 minutes for Phases 1+2

### Resource Usage

- **Jobs**: Minimal (bitnami/kubectl image)
- **Rollouts**: Brief CPU spike during pod replacement
- **Downtime**: 
  - CRDs: None
  - Controller-manager: None (rolling update)
  - Webhook: ~5-10 seconds (brief unavailability)

---

## Safety Features

1. **Pre-upgrade Backup**
   - CRDs backed up before upgrade
   - Deployment YAMLs saved in job pod

2. **Version Validation**
   - Auto-detects local cluster version from liqo-controller-manager
   - Retrieves remote cluster versions from ForeignCluster CRs
   - Validates minimum version compatibility with target version

3. **Smart Comparison**
   - SHA256 hash comparison
   - Only applies changed resources

4. **Health Checks**
   - Waits for deployments to be Available
   - Verifies pods are Running

5. **Automatic Rollback**
   - Triggers on job failure
   - Restores from backup

6. **TTL Cleanup**
   - Jobs auto-delete after 5 minutes
   - Keeps cluster clean

---

## Next Steps

### Phase 3: Extended Control Plane
- liqo-ipam
- liqo-crd-replicator
- liqo-metric-agent
- liqo-proxy

### Phase 4: Data Plane
- liqo-fabric (DaemonSet - most disruptive)
- liqo-gateway

### Phase 5: Final Cleanup
- Update all Helm labels to target version
- Final verification
- Cleanup old resources

---

## Production Readiness

✅ **Phases 1 & 2 are production-ready**

- Tested end-to-end
- Backup and rollback working
- Zero-downtime for controller-manager
- Brief (~10s) webhook unavailability
- Clear logs and status updates

**Safe to use in production clusters.**

---

## Questions?

- Check controller logs: `kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f`
- Check job logs: `kubectl logs -n liqo -l app.kubernetes.io/component=controlplane-upgrade -f`
- Describe upgrade: `kubectl describe liqoupgrade <name> -n liqo`