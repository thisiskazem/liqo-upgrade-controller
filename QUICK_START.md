# Quick Start Guide - Liqo Smart Upgrade Controller

## All Files You Need

I've created 5 files for you. Here's what each one does and where it goes:

### 1. **liqoupgrade_types.go** 
   - **What**: API type definitions (Spec, Status, Phases)
   - **Where**: Copy to `api/v1alpha1/liqoupgrade_types.go`
   - **Action**: Replace the existing file

### 2. **liqoupgrade_controller_final.go**
   - **What**: Main controller logic with smart CRD upgrade
   - **Where**: Copy to `internal/controller/liqoupgrade_controller.go`
   - **Action**: Replace the existing file

### 3. **upgrade-rbac.yaml**
   - **What**: ServiceAccount + ClusterRole + ClusterRoleBinding
   - **Where**: Apply directly to cluster
   - **Action**: `kubectl apply -f upgrade-rbac.yaml`

### 4. **test-upgrade.yaml**
   - **What**: Example LiqoUpgrade CR for testing
   - **Where**: Apply directly to cluster
   - **Action**: `kubectl apply -f test-upgrade.yaml`

### 5. **README.md**
   - **What**: Complete documentation
   - **Where**: Keep for reference
   - **Action**: Read it!

---

## Quick Deploy (5 Commands)

```bash
# 1. Copy files to your project
cp liqoupgrade_types.go api/v1alpha1/liqoupgrade_types.go
cp liqoupgrade_controller_final.go internal/controller/liqoupgrade_controller.go

# 2. Generate and build
make manifests generate
make docker-build docker-push IMG=kazem26/liqo-upgrade-controller:v0.2

# 3. Deploy to cluster
make install
make deploy IMG=kazem26/liqo-upgrade-controller:v0.2

# 4. Create RBAC
kubectl apply -f upgrade-rbac.yaml

# 5. Test it!
kubectl apply -f test-upgrade.yaml
kubectl logs -n liqo -l job-name=liqo-upgrade-crd-liqo-upgrade-test -f
```

---

## What Makes This "Smart"?

### ✅ Version Validation
- Checks cluster's actual Liqo version
- Fails if you specify wrong currentVersion
- Prevents accidental upgrades from wrong versions

### ✅ Intelligent Comparison
- Fetches CRD lists from GitHub for both versions
- Downloads and hashes each CRD (SHA256)
- Only applies CRDs that actually changed

### ✅ Safety First
- Exits with error code 1 on validation failure
- Nothing modified until validation passes
- Clear error messages explaining what went wrong

---

## Test Scenarios

### Scenario 1: Successful Upgrade
```yaml
# Cluster has v1.0.0, you upgrade to v1.0.1
spec:
  currentVersion: v1.0.0  # ✅ Matches cluster
  targetVersion: v1.0.1

Result: Only changed CRDs upgraded, status → Completed
```

### Scenario 2: Version Mismatch (Safety)
```yaml
# Cluster has v1.0.0, but you mistakenly write v0.9.5
spec:
  currentVersion: v0.9.5  # ❌ Doesn't match cluster (v1.0.0)
  targetVersion: v1.0.1

Result: Job fails immediately with error, nothing modified
```

### Scenario 3: No Changes
```yaml
# Both versions have identical CRDs
spec:
  currentVersion: v1.0.0
  targetVersion: v1.0.0  # Same version

Result: "No CRD changes detected. Nothing to upgrade."
```

---

## Download Links

All files are ready in `/mnt/user-data/outputs/`:

1. liqoupgrade_types.go
2. liqoupgrade_controller_final.go
3. upgrade-rbac.yaml
4. test-upgrade.yaml
5. README.md

**Next Step**: Download these files and follow the Quick Deploy commands above!