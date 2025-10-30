# Liqo Upgrade Controller - Quick Start Guide

**Zero to upgraded Liqo in 5 minutes** ⚡

---

## Prerequisites

- Kubernetes cluster with Liqo v1.0.0 installed
- `kubectl` configured
- Docker (for building controller image)
- Docker Hub account or private registry

---

## Quick Setup (3 Commands)

### 1. Setup Project
```bash
cd liqo-upgrade-controller

# Copy all required files
cp liqoupgrade_types.go api/v1alpha1/
cp liqoupgradebackup_types.go api/v1alpha1/
cp liqoupgrade_controller_phase2_fixed.go internal/controller/liqoupgrade_controller.go
cp kustomization.yaml config/crd/
```

### 2. Build & Deploy
```bash
# Generate manifests and build image
make manifests generate
make docker-build docker-push IMG=<YOUR_REGISTRY>/liqo-upgrade-controller:v0.3

# Deploy to cluster
make deploy IMG=<YOUR_REGISTRY>/liqo-upgrade-controller:v0.3
kubectl apply -f upgrade-rbac.yaml
```

### 3. Run Upgrade
```bash
# Start upgrade
kubectl apply -f test-upgrade.yaml

# Watch progress (Ctrl+C to exit)
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f
```

---

## What Happens?

### Phase 1: CRDs (~2-3 min)
```
✓ Backup all 30 CRDs
✓ Compare SHA256 hashes
✓ Upgrade 6 changed CRDs
✓ Verify upgrade success
```

### Phase 2: Control Plane (~3-5 min)
```
✓ Backup deployments
✓ Upgrade liqo-controller-manager (v1.0.0 → v1.0.1)
✓ Upgrade liqo-webhook (v1.0.0 → v1.0.1)
✓ Health checks pass
```

**Total Time:** 5-8 minutes

---

## Verify Success

```bash
# Check upgrade status
kubectl get liqoupgrade -n liqo

# Expected output:
# NAME                CURRENT   TARGET   PHASE       AGE
# liqo-upgrade-test   v1.0.0    v1.0.1   Completed   8m

# Verify component versions
kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
# Output: ghcr.io/liqotech/liqo-controller-manager:v1.0.1

kubectl get deployment liqo-webhook -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
# Output: ghcr.io/liqotech/webhook:v1.0.1
```

---

## File Reference

| File | Purpose |
|------|---------|
| `liqoupgrade_types.go` | Main upgrade CRD definition |
| `liqoupgradebackup_types.go` | Backup CRD definition |
| `liqoupgrade_controller_phase2_fixed.go` | Controller logic (Phases 1+2) |
| `kustomization.yaml` | CRD kustomization config |
| `upgrade-rbac.yaml` | ServiceAccount + RBAC rules |
| `test-upgrade.yaml` | Example upgrade CR |

---

## Troubleshooting

### Issue: RBAC Permission Denied
```bash
# Error: "cannot watch deployments"
# Fix:
kubectl apply -f upgrade-rbac.yaml
```

### Issue: Wrong Image Version
```bash
# Check what's running
kubectl get pods -n liqo -o wide

# Check image in deployment
kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}'
```

### Issue: Upgrade Stuck
```bash
# Check controller logs
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager --tail=100

# Check job logs
kubectl get jobs -n liqo
kubectl logs -n liqo <job-name>

# Clean and retry
kubectl delete liqoupgrade --all -n liqo
kubectl delete jobs -n liqo --all
kubectl apply -f test-upgrade.yaml
```

---

## Next Steps

✅ **Phases 1 & 2 Complete** - CRDs + Core Control Plane upgraded

Coming next:
- 🚧 **Phase 3**: Extended Control Plane (ipam, crd-replicator, metric-agent, proxy)
- 🚧 **Phase 4**: Data Plane (fabric, gateway)
- 🚧 **Phase 5**: Final Cleanup (label updates, verification)

---

## Production Use

**Ready for production?** Yes for Phases 1 & 2!

✅ Backup and rollback tested
✅ Zero downtime (except brief webhook unavailability)
✅ Automatic health checks
✅ Auto-cleanup of job resources

**Recommendation:** Test in staging first, then apply to production.

---

## Support

**Check status:**
```bash
kubectl describe liqoupgrade <name> -n liqo
```

**View all jobs:**
```bash
kubectl get jobs -n liqo
```

**See controller logs:**
```bash
kubectl logs -n liqo-upgrade-controller-system -l control-plane=controller-manager -f
```

**Full README:** See `README.md` for detailed documentation.