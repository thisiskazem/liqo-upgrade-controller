# Liqo Upgrade Controller - Phase 1: Smart CRD Upgrade

## Overview

This controller performs **smart CRD upgrades** for Liqo with:
- ✅ Version validation (fails if currentVersion doesn't match cluster)
- ✅ SHA256 comparison (only upgrades changed CRDs)
- ✅ GitHub API integration (fetches CRD lists dynamically)
- ✅ Detailed logging and error handling

---

## Files in This Codebase

### Core Files
1. **liqoupgrade_types.go** - API definitions (goes in `api/v1alpha1/`)
2. **liqoupgrade_controller_final.go** - Controller logic (goes in `internal/controller/liqoupgrade_controller.go`)

### Deployment Files
3. **upgrade-rbac.yaml** - ServiceAccount and permissions (apply to cluster)
4. **test-upgrade.yaml** - Test upgrade manifest (apply to cluster)

---

## Deployment Steps

### Step 1: Update Your Code

```bash
# Copy the types file
cp liqoupgrade_types.go api/v1alpha1/liqoupgrade_types.go

# Copy the controller file
cp liqoupgrade_controller_final.go internal/controller/liqoupgrade_controller.go

# Generate manifests
make manifests
make generate
```

### Step 2: Build and Push Docker Image

```bash
# Build and push (replace with your Docker Hub username)
make docker-build docker-push IMG=kazem26/liqo-upgrade-controller:v0.2
```

### Step 3: Deploy to Cluster

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=kazem26/liqo-upgrade-controller:v0.2

# Verify controller is running
kubectl get pods -n liqo-upgrade-controller-system
```

### Step 4: Create ServiceAccount and RBAC

```bash
# Apply RBAC for the job's ServiceAccount
kubectl apply -f upgrade-rbac.yaml

# Verify
kubectl get sa liqo-upgrade-controller -n liqo
kubectl get clusterrole liqo-upgrade-job-role
kubectl get clusterrolebinding liqo-upgrade-job-binding
```

### Step 5: Test the Upgrade

```bash
# Apply the upgrade request
kubectl apply -f test-upgrade.yaml

# Watch the upgrade progress
kubectl get liqoupgrade -n liqo -w

# Check the job
kubectl get jobs -n liqo

# Watch logs (this shows the smart comparison)
kubectl logs -n liqo -l job-name=liqo-upgrade-crd-liqo-upgrade-test -f
```

---

## What Happens During Upgrade

### Phase 0: Version Validation
```
Step 0: Validating current version...
  User specified: v1.0.0
  Cluster has: v1.0.0
  ✅ Version validation passed!
```

**If mismatch:**
```
❌ ERROR: Version mismatch detected!

  You specified currentVersion: v1.0.0
  But cluster actually has: v0.9.5

  Please update your LiqoUpgrade CR with the correct currentVersion.

[Job exits with code 1]
```

### Phase 1: Fetch CRD Lists
```
Step 1: Fetching CRD lists from GitHub...
  Found 15 unique CRDs
```

### Phase 2: Smart Comparison
```
Step 2: Comparing CRDs between versions...
  Checking: core.liqo.io_foreignclusters.yaml
    → CHANGED (adding to upgrade list)
  Checking: networking.liqo.io_configurations.yaml
    → No changes
  Checking: offloading.liqo.io_namespacemaps.yaml
    → CHANGED (adding to upgrade list)
```

### Phase 3: Apply Only Changed CRDs
```
Step 3: Applying changed/new CRDs...
  Applying: core.liqo.io_foreignclusters.yaml
  Applying: offloading.liqo.io_namespacemaps.yaml

✅ CRD upgrade completed successfully!
```

---

## Expected Output

### Successful Upgrade
```bash
$ kubectl get liqoupgrade -n liqo
NAME                CURRENT   TARGET   PHASE       AGE
liqo-upgrade-test   v1.0.0    v1.0.1   Completed   2m
```

### Failed Upgrade (Version Mismatch)
```bash
$ kubectl get liqoupgrade -n liqo
NAME                CURRENT   TARGET   PHASE    AGE
liqo-upgrade-test   v1.0.0    v1.0.1   Failed   30s

$ kubectl describe liqoupgrade liqo-upgrade-test -n liqo
Status:
  Phase: Failed
  Message: CRD upgrade job failed - check job logs for details
```

Check logs:
```bash
$ kubectl logs -n liqo -l job-name=liqo-upgrade-crd-liqo-upgrade-test
❌ ERROR: Version mismatch detected!
  You specified currentVersion: v1.0.0
  But cluster actually has: v0.9.5
```

---

## Troubleshooting

### Job Pod Not Starting
```bash
# Check events
kubectl describe job liqo-upgrade-crd-liqo-upgrade-test -n liqo

# Common issue: ServiceAccount missing
Error: pods "liqo-upgrade-crd-liqo-upgrade-test-" is forbidden: 
       serviceaccount "liqo-upgrade-controller" not found

# Fix: Apply RBAC
kubectl apply -f upgrade-rbac.yaml
```

### Job Fails Immediately
```bash
# Check logs
kubectl logs -n liqo -l job-name=liqo-upgrade-crd-liqo-upgrade-test

# Common issues:
# 1. Version mismatch → Update currentVersion in test-upgrade.yaml
# 2. Network error → Check cluster can reach github.com
# 3. Permission denied → Check RBAC is applied
```

### Clean Up and Retry
```bash
# Delete the upgrade resource (this also deletes the job)
kubectl delete liqoupgrade liqo-upgrade-test -n liqo

# Wait a few seconds, then reapply
kubectl apply -f test-upgrade.yaml
```

---

## Key Features

### 1. Version Validation
- Reads actual version from cluster CRDs
- Compares with user-specified `currentVersion`
- **Fails fast** if mismatch detected (exit code 1)

### 2. Smart Comparison
- Fetches CRD lists from GitHub API
- Downloads both versions
- Uses SHA256 hash comparison
- Only applies changed/new CRDs

### 3. Safety
- Read-only until comparison complete
- No modifications if versions don't match
- Idempotent (safe to re-run)

### 4. Observability
- Detailed logs for each step
- Clear error messages
- Status updates in CR

---

## Next Steps (Future Phases)

- **Phase 2**: Control Plane upgrade (controller-manager, webhook, etc.)
- **Phase 3**: Data Plane upgrade (virtual-kubelet, gateway, etc.)
- **Phase 4**: Verification and rollback logic
- **Phase 5**: Multi-cluster coordination

---

## Questions?

- Check controller logs: `kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f`
- Check job logs: `kubectl logs -n liqo -l job-name=liqo-upgrade-crd-liqo-upgrade-test -f`
- Describe upgrade: `kubectl describe liqoupgrade liqo-upgrade-test -n liqo`