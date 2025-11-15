# Liqo Upgrade Controller - Stages 0-3 Complete ✅

## Overview

**Production-grade Liqo upgrade** with validation, backup, rollback, and health checks:

- ✅ **Stage 0: Validation & Freeze** - Multi-cluster version validation and operation freeze
- ✅ **Stage 1: CRDs** - All Liqo CRDs upgraded (15+ CRDs)
- ✅ **Stage 2: Controller Manager** - liqo-controller-manager upgraded
- ✅ **Stage 3: Network Fabric** - Gateway templates, liqo-ipam, liqo-proxy, liqo-fabric, and gateway instances upgraded
- ✅ **Verification** - Health checks and version validation
- ✅ **Rollback** - Automatic rollback on failure

---

## What Works Now

### Stage 0: Validation & Freeze Operations (Complete)
- Cluster identity verification (checks liqo-controller-manager exists)
- Local cluster version detection from controller-manager image
- Remote cluster version retrieval from ForeignCluster CRs
- Minimum version calculation across all clusters
- Version compatibility matrix validation (ConfigMap-based)
- Critical environment variables backup (POD_NAMESPACE, CLUSTER_ID, TENANT_NAMESPACE, etc.)
- Component health checks
- Freeze new offloads and peerings

### Stage 1: CRD Upgrade (Complete)
- Fetches CRD list from GitHub for target version
- Downloads each CRD YAML from GitHub raw content
- Server-side apply with conflict resolution (--server-side --force-conflicts)
- CRD establishment verification
- Validates minimum 15 Liqo CRDs are present

### Stage 2: Controller Manager Upgrade (Complete)
- Backs up current deployment spec to /tmp/
- Extracts and preserves critical environment variables
- Updates image using kubectl set image
- Waits for rollout completion (5 minute timeout)
- Verifies environment variables were preserved
- Deployment health checks (2 minute timeout)
- Version verification (deployed == target)

### Stage 3: Network Fabric Upgrade (Complete)
- Backs up gateway deployments from liqo-tenant-* namespaces
- Backs up liqo-fabric DaemonSet, liqo-ipam, liqo-proxy Deployments
- Updates WgGatewayClientTemplate and WgGatewayServerTemplate with new images
- Sequential component upgrade:
  - liqo-ipam (IP allocation manager)
  - liqo-proxy (Proxy component)
  - liqo-fabric (Data plane DaemonSet)
- Triggers gateway instance recreation by annotating and deleting resources
- Monitors deployment rollouts in tenant namespaces
- Verifies all components running target version

### Verification Phase (Complete)
- Component health verification
- Deployed version validation (must match target)
- Adds "Healthy" condition to status
- Marks upgrade as completed or triggers rollback

### Rollback Phase (Complete)
- Auto-rollback enabled by default (configurable)
- Rolls back liqo-controller-manager if upgraded
- Rolls back network fabric components if upgraded (gateway templates, ipam, proxy, fabric)
- Uses lastSuccessfulPhase for partial rollback
- Restores from environment backup ConfigMap
- Verifies environment variables and health after rollback

---

## Architecture

### Components Upgraded

**Stage 0 (Validation & Freeze):**
- Version compatibility validation across multi-cluster setup
- Environment configuration backup
- Operation freeze (offloads, peerings)

**Stage 1 (CRDs):**
- All Liqo CustomResourceDefinitions (15+ CRDs fetched from GitHub)

**Stage 2 (Controller Manager):**
- `liqo-controller-manager` - Core orchestrator

**Stage 3 (Network Fabric):**
1. Gateway Templates:
   - `WgGatewayClientTemplate` - Client gateway template
   - `WgGatewayServerTemplate` - Server gateway template
2. Network Components:
   - `liqo-ipam` - IP allocation manager (Deployment)
   - `liqo-proxy` - Proxy component (Deployment)
   - `liqo-fabric` - Data plane component (DaemonSet)
3. Gateway Instances:
   - `GatewayClient` / `WgGatewayClient` in tenant namespaces
   - `GatewayServer` / `WgGatewayServer` in tenant namespaces

### Not Yet Upgraded

**Extended Control Plane:**
- liqo-webhook (admission webhook)
- liqo-crd-replicator
- liqo-metric-agent
- Other control plane components

**Final Cleanup:**
- Helm label updates
- Final resource cleanup

---

## Files

### Controller Structure

The controller is modular with each stage in its own file:

**API Types:**
- `api/v1alpha1/liqoupgrade_types.go` - LiqoUpgrade CRD definition

**Main Controller:**
- `internal/controller/liqoupgrade_controller.go` - Main reconciliation loop and state machine

**Stage Implementations:**
- `internal/controller/validation.go` - Stage 0: Validation phase
- `internal/controller/freeze_operations.go` - Stage 0: Freeze operations phase
- `internal/controller/crd_upgrade.go` - Stage 1: CRD upgrade
- `internal/controller/controller_manager_upgrade.go` - Stage 2: Controller manager upgrade
- `internal/controller/network_fabric_upgrade.go` - Stage 3: Network fabric upgrade
- `internal/controller/verification.go` - Verification phase
- `internal/controller/rollback.go` - Rollback phase

**Utilities:**
- `internal/controller/utils.go` - Shared utility functions (job creation, health checks, etc.)

**Configuration:**
- `config/rbac/` - RBAC permissions
- `config/crd/` - CRD definitions

---

## Deployment

### Prerequisites
- Liqo installed (any version)
- kubectl access to the cluster
- Container registry access for pushing controller image
- Version compatibility matrix ConfigMap (optional, for validation)

### Build & Deploy

```bash
cd liqo-upgrade-controller

# Generate CRDs and manifests
make manifests generate

# Build and push controller image
make docker-build docker-push IMG=<your-registry>/liqo-upgrade-controller:v0.4

# Deploy to cluster
make deploy IMG=<your-registry>/liqo-upgrade-controller:v0.4
```

### Create Upgrade CR

```bash
# Create LiqoUpgrade resource
cat <<EOF | kubectl apply -f -
apiVersion: upgrade.liqo.io/v1alpha1
kind: LiqoUpgrade
metadata:
  name: liqo-upgrade-test
  namespace: liqo
spec:
  targetVersion: "v0.10.3"  # Target Liqo version
  autoRollback: true         # Enable automatic rollback on failure
EOF

# Watch progress
kubectl get liqoupgrade -n liqo -w

# Check detailed status
kubectl describe liqoupgrade liqo-upgrade-test -n liqo

# View controller logs
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f
```

---

## Complete Upgrade Flow

```
User applies LiqoUpgrade CR
     ↓
Stage 0: Validation (PhaseValidating) - 30s
  └─> Verify cluster identity (liqo-controller-manager exists)
  └─> Detect local cluster version from controller-manager image
  └─> Retrieve remote cluster versions from ForeignCluster CRs
  └─> Find minimum version across all clusters
  └─> Validate compatibility using version matrix ConfigMap
  └─> Backup critical environment variables to ConfigMap
  └─> Component health checks
     ↓
Stage 0: Freeze Operations (PhaseFreezingOperations) - 10s
  └─> Block new offloads and peerings
     ↓
Stage 1: CRD Upgrade (PhaseCRDs) - 1-2min
  └─> Fetch CRD list from GitHub for target version
  └─> Download each CRD YAML from GitHub raw content
  └─> Apply CRDs using server-side apply (--server-side --force-conflicts)
  └─> Verify CRDs are established
  └─> Validate minimum 15 Liqo CRDs present
     ↓
Stage 2: Controller Manager (PhaseControllerManager) - 3-5min
  └─> Backup current deployment spec to /tmp/
  └─> Extract and preserve critical env vars
  └─> Update image using kubectl set image
  └─> Wait for rollout completion (5 min timeout)
  └─> Verify environment variables preserved
  └─> Check deployment health (2 min timeout)
  └─> Verify deployed version matches target
     ↓
Stage 3: Network Fabric (PhaseNetworkFabric) - 5-10min
  └─> Backup network components (gateways, fabric, ipam, proxy)
  └─> Update WgGatewayClientTemplate with new images (gateway, wireguard, geneve)
  └─> Update WgGatewayServerTemplate with new images
  └─> Verify templates updated to target version
  └─> Upgrade liqo-ipam Deployment
  └─> Upgrade liqo-proxy Deployment
  └─> Upgrade liqo-fabric DaemonSet (data plane)
  └─> Annotate GatewayClient/GatewayServer to trigger reconciliation
  └─> Delete WgGatewayClient/WgGatewayServer to force recreation
  └─> Wait for controller to recreate with new version
  └─> Monitor deployment rollouts in tenant namespaces
  └─> Verify all components running target version
     ↓
Verification (PhaseVerifying) - 1min
  └─> Verify all components healthy
  └─> Verify deployed version matches target
  └─> Add "Healthy" condition to status
     ↓
Status: Completed (PhaseCompleted) ✅

OR on failure:
     ↓
Rollback (PhaseRollingBack) - 3-5min
  └─> Roll back liqo-controller-manager (if upgraded)
  └─> Roll back network fabric (if upgraded)
      └─> Restore gateway templates
      └─> Restore liqo-ipam
      └─> Restore liqo-proxy
      └─> Restore liqo-fabric
      └─> Restore gateway instances
  └─> Restore environment variables from backup ConfigMap
  └─> Verify environment variables and health
     ↓
Status: Failed (PhaseFailed) ❌
```

---

## Expected Results

### Successful Upgrade

```bash
$ kubectl get liqoupgrade -n liqo
NAME                PHASE       AGE
liqo-upgrade-test   Completed   15m

$ kubectl get liqoupgrade liqo-upgrade-test -n liqo -o yaml
status:
  phase: Completed
  message: "Upgrade completed successfully"
  currentVersion: "v0.10.3"
  lastSuccessfulPhase: PhaseVerifying
  conditions:
  - type: Healthy
    status: "True"
    reason: AllComponentsHealthy
```

### Verify Component Versions

```bash
# Check controller-manager
$ kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/liqo-controller-manager:v0.10.3

# Check network fabric
$ kubectl get daemonset liqo-fabric -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/liqo-fabric:v0.10.3

$ kubectl get deployment liqo-ipam -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/liqo-ipam:v0.10.3

$ kubectl get deployment liqo-proxy -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
ghcr.io/liqotech/liqo-proxy:v0.10.3

# Check gateway templates
$ kubectl get wggatewayservertemplate -n liqo -o yaml | grep image:
# Should show v0.10.3 for gateway, wireguard, and geneve containers

# Check running pods
$ kubectl get pods -n liqo
NAME                                       READY   STATUS    AGE
liqo-controller-manager-xyz                1/1     Running   10m
liqo-fabric-abc                            1/1     Running   8m
liqo-ipam-def                              1/1     Running   8m
liqo-proxy-ghi                             1/1     Running   8m

# Check gateway pods in tenant namespaces
$ kubectl get pods -n liqo-tenant-*
# Should show gateway pods with new version
```

---

## Jobs Created

All jobs have `ttlSecondsAfterFinished: 300` (auto-delete after 5 minutes):

1. **liqo-validate-*** - Stage 0: Validates cluster and versions
2. **liqo-freeze-*** - Stage 0: Freezes operations
3. **liqo-upgrade-crd-*** - Stage 1: Upgrades CRDs
4. **liqo-upgrade-controller-manager-*** - Stage 2: Upgrades controller-manager
5. **liqo-upgrade-network-fabric-*** - Stage 3: Upgrades network fabric
6. **liqo-verify-*** - Verification: Verifies health and version
7. **liqo-rollback-*** (on failure) - Rollback: Restores previous version

Jobs use `bitnami/kubectl` image and run bash scripts generated by the controller.

---

## Rollback

### Automatic Rollback

The controller has **smart rollback** based on `lastSuccessfulPhase`:

**If validation fails:**
- No rollback needed (nothing was changed)

**If CRD upgrade fails:**
- No component rollback (CRDs are additive and backward compatible)
- Status marked as Failed

**If Controller Manager upgrade fails:**
- Rolls back liqo-controller-manager to previous version
- Restores environment variables from backup ConfigMap

**If Network Fabric upgrade fails:**
- Rolls back liqo-controller-manager (if it was upgraded)
- Rolls back network fabric components:
  - Gateway templates (WgGatewayClientTemplate, WgGatewayServerTemplate)
  - liqo-ipam Deployment
  - liqo-proxy Deployment
  - liqo-fabric DaemonSet
  - Gateway instances in tenant namespaces
- Restores environment variables from backup ConfigMap

**Rollback is enabled by default** and can be disabled by setting `spec.autoRollback: false` in the LiqoUpgrade CR.

### Manual Rollback

If automatic rollback fails or is disabled:

```bash
# Rollback controller-manager
kubectl set image deployment/liqo-controller-manager \
  controller-manager=ghcr.io/liqotech/liqo-controller-manager:v0.10.2 -n liqo

# Rollback network fabric
kubectl set image daemonset/liqo-fabric \
  liqo-fabric=ghcr.io/liqotech/liqo-fabric:v0.10.2 -n liqo

kubectl set image deployment/liqo-ipam \
  ipam=ghcr.io/liqotech/liqo-ipam:v0.10.2 -n liqo

kubectl set image deployment/liqo-proxy \
  liqo-proxy=ghcr.io/liqotech/liqo-proxy:v0.10.2 -n liqo

# Rollback gateway templates
kubectl patch wggatewayservertemplate <name> -n liqo --type=json \
  -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/liqonet:v0.10.2"}]'
# Repeat for WgGatewayClientTemplate and all container images
```

---

## Troubleshooting

### Check Upgrade Status

```bash
# Get current phase
kubectl get liqoupgrade -n liqo

# Detailed status
kubectl describe liqoupgrade <name> -n liqo

# Controller logs
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f

# Job logs (find the failing job)
kubectl get jobs -n liqo
kubectl logs job/<job-name> -n liqo
```

### Validation Fails

```bash
# Error: Version compatibility check failed
# Check the version matrix ConfigMap
kubectl get configmap liqo-version-matrix -n liqo -o yaml

# Error: ForeignCluster version detection failed
# Check ForeignCluster resources
kubectl get foreignclusters -A
kubectl get foreignclusters <name> -n liqo -o yaml

# Error: liqo-controller-manager not found
# Ensure Liqo is installed
kubectl get deployment liqo-controller-manager -n liqo
```

### Job Fails: RBAC Issues

```bash
# Error: cannot get/patch/update resources
# Check controller RBAC permissions
kubectl describe clusterrole liqo-upgrade-controller-manager-role

# Verify ServiceAccount
kubectl get sa -n liqo-upgrade-controller-system
```

### Deployment Stuck Rolling Out

```bash
# Check deployment status
kubectl describe deployment liqo-controller-manager -n liqo
kubectl describe daemonset liqo-fabric -n liqo

# Check pods
kubectl get pods -n liqo
kubectl describe pod <pod-name> -n liqo
kubectl logs <pod-name> -n liqo

# Common issues:
# - ImagePullBackOff: Wrong image name/tag or registry auth
# - CrashLoopBackOff: Check pod logs for errors
# - Pending: Check resource constraints or node selectors
```

### Network Fabric Upgrade Fails

```bash
# Check gateway templates
kubectl get wggatewayservertemplate -n liqo -o yaml
kubectl get wggatewayclienttemplate -n liqo -o yaml

# Check gateway instances in tenant namespaces
kubectl get gatewayclient,gatewayserver -A
kubectl get wggatewayclient,wggatewayserver -A

# Check network component logs
kubectl logs deployment/liqo-ipam -n liqo
kubectl logs deployment/liqo-proxy -n liqo
kubectl logs daemonset/liqo-fabric -n liqo
```

### Clean Retry

```bash
# Delete the LiqoUpgrade resource
kubectl delete liqoupgrade <name> -n liqo

# Wait for jobs to be cleaned up (TTL: 5 minutes)
# Or manually delete
kubectl delete jobs -n liqo -l app.kubernetes.io/managed-by=liqo-upgrade-controller

# Ensure all components are healthy
kubectl get deployments -n liqo
kubectl get daemonsets -n liqo
kubectl rollout status deployment/liqo-controller-manager -n liqo

# Retry upgrade
kubectl apply -f <your-upgrade-cr.yaml>
```

---

## Known Issues & Notes

### Helm Labels Not Updated

After upgrade, deployment labels still show old version:
```yaml
labels:
  app.kubernetes.io/version: v0.10.2  # Still shows old version
  helm.sh/chart: liqo-v0.10.2
```

**Why:** `kubectl set image` only changes container images, not labels.

**Impact:** None - labels are metadata only. Container images are correct.

**Fix:** Will be addressed in future final cleanup phase.

### Gateway Recreation

During Stage 3 (Network Fabric), gateway pods in tenant namespaces are recreated. This causes:
- Brief network interruption for cross-cluster traffic
- Gateway pods get new names
- Typically takes 30-60 seconds to reconnect

**Mitigation:** The controller waits for all gateways to be Ready before completing the stage.

### CRD Schema Changes

CRD upgrades may introduce new fields or deprecate old ones. The controller uses server-side apply to handle this safely, but:
- Existing resources are not automatically migrated
- Custom fields in existing resources are preserved
- Check Liqo release notes for manual migration steps if needed

---

## Performance

### Typical Timing

- **Stage 0 (Validation & Freeze)**: ~30-40 seconds
- **Stage 1 (CRDs)**: ~1-2 minutes
- **Stage 2 (Controller Manager)**: ~3-5 minutes
- **Stage 3 (Network Fabric)**: ~5-10 minutes (depends on number of peerings)
- **Verification**: ~1 minute
- **Total**: ~10-18 minutes for complete upgrade

### Resource Usage

- **Jobs**: Minimal (bitnami/kubectl image, ~50MB memory)
- **Rollouts**: Brief CPU spike during pod replacement
- **Network**: Download CRDs from GitHub (~1-2MB total)

### Downtime

- **CRDs**: None (additive changes)
- **Controller-manager**: None (rolling update with readiness probes)
- **Network Fabric**:
  - liqo-ipam: None (rolling update)
  - liqo-proxy: None (rolling update)
  - liqo-fabric: Brief disruption during DaemonSet rolling update (~10-30 seconds per node)
  - Gateways: Brief disruption during recreation (~30-60 seconds per peering)

---

## Safety Features

1. **Multi-cluster Version Validation**
   - Auto-detects local cluster version from liqo-controller-manager image
   - Retrieves remote cluster versions from ForeignCluster CRs
   - Calculates minimum version across all clusters for compatibility
   - Validates against version compatibility matrix (ConfigMap)
   - Prevents incompatible upgrades

2. **Environment Configuration Backup**
   - Critical environment variables backed up to ConfigMap before upgrade
   - Variables: POD_NAMESPACE, CLUSTER_ID, TENANT_NAMESPACE, CLUSTER_ROLE, ENABLE_IPAM, LOG_LEVEL
   - Restored during rollback or verified after upgrade

3. **Component Health Checks**
   - Pre-upgrade health verification
   - Continuous health monitoring during rollouts (2-5 minute timeouts)
   - Post-upgrade health validation
   - Checks ReadyReplicas >= 1 for all deployments

4. **Server-side Apply for CRDs**
   - Uses `kubectl apply --server-side --force-conflicts` for safe CRD updates
   - Handles field ownership conflicts automatically
   - Preserves existing custom fields in resources

5. **Gradual Rollout**
   - Sequential upgrade of components (controller-manager → network fabric)
   - Uses Kubernetes native rolling updates (zero downtime for deployments)
   - Waits for each component to be healthy before proceeding

6. **Automatic Rollback**
   - Enabled by default (configurable via spec.autoRollback)
   - Triggered on job failure or health check failure
   - Partial rollback based on lastSuccessfulPhase (only rolls back what was upgraded)
   - Restores from environment backup ConfigMap

7. **Phase Tracking**
   - Tracks last successful phase in status
   - Enables partial rollback (doesn't roll back successful stages)
   - Clear status reporting with conditions

8. **Job Timeouts & TTL**
   - Jobs have execution timeouts (5-10 minutes depending on stage)
   - Jobs auto-delete after 5 minutes (TTL) for cleanup
   - Prevents hanging operations

9. **Version Verification**
   - Post-upgrade version verification (deployed must match target)
   - Checks both deployment specs and running pod images
   - Triggers rollback if version mismatch detected

---

## Next Steps

### Additional Control Plane Components
- liqo-webhook (admission webhook)
- liqo-crd-replicator
- liqo-metric-agent
- Any other control plane components not yet covered

### Final Cleanup Phase
- Update all Helm labels to target version
- Update chart annotations
- Final resource cleanup
- Comprehensive end-to-end verification

### Enhancements
- Pre-upgrade backup of critical resources to external storage
- Support for custom pre/post-upgrade hooks
- Progress reporting with percentage completion
- Dry-run mode for testing upgrade compatibility
- Support for upgrading multiple clusters in a federation

---

## Production Readiness

✅ **Stages 0-3 are implemented and functional**

**Implemented Features:**
- Multi-cluster version validation
- CRD upgrade with server-side apply
- Controller-manager upgrade with environment preservation
- Network fabric upgrade (ipam, proxy, fabric, gateways)
- Automatic rollback on failure
- Health checks and verification
- Clean status reporting

**Testing Status:**
- Basic functionality tested
- Rollback mechanism tested
- Health check validation tested

**Production Considerations:**
- Test thoroughly in staging environment first
- Review Liqo release notes for breaking changes
- Ensure version compatibility matrix is configured
- Monitor logs during upgrade
- Have rollback plan ready
- Expect brief network disruption during gateway recreation (~30-60s)

**Recommended for:**
- Development and testing environments
- Staging environments with controlled testing
- Production with proper validation and testing

---

## Questions?

- Check controller logs: `kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f`
- Check job logs: `kubectl logs -n liqo -l app.kubernetes.io/component=controlplane-upgrade -f`
- Describe upgrade: `kubectl describe liqoupgrade <name> -n liqo`