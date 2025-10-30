/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// LiqoUpgradeReconciler reconciles a LiqoUpgrade object
type LiqoUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	finalizerName     = "upgrade.liqo.io/finalizer"
	backupJobPrefix   = "liqo-backup"
	upgradeJobPrefix  = "liqo-upgrade-crd"
	verifyJobPrefix   = "liqo-verify-crd"
	rollbackJobPrefix = "liqo-rollback-crd"
)

// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgradebackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgradebackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *LiqoUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the LiqoUpgrade instance
	upgrade := &upgradev1alpha1.LiqoUpgrade{}
	if err := r.Get(ctx, req.NamespacedName, upgrade); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("LiqoUpgrade resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get LiqoUpgrade")
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !upgrade.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, upgrade)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(upgrade, finalizerName) {
		controllerutil.AddFinalizer(upgrade, finalizerName)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	// State machine based on current phase
	logger.Info("Reconciling upgrade", "currentPhase", upgrade.Status.Phase)

	switch upgrade.Status.Phase {
	case "":
		logger.Info("Phase is empty, starting backup")
		return r.startBackup(ctx, upgrade)
	case upgradev1alpha1.PhaseBackup:
		return r.monitorBackup(ctx, upgrade)
	case upgradev1alpha1.PhaseCRDs:
		return r.monitorCRDUpgrade(ctx, upgrade)
	case upgradev1alpha1.PhaseVerifyingCRDs:
		return r.monitorCRDVerification(ctx, upgrade)
	case upgradev1alpha1.PhaseControlPlane:
		return r.monitorControlPlaneUpgrade(ctx, upgrade)
	case upgradev1alpha1.PhaseRollingBack:
		return r.monitorRollback(ctx, upgrade)
	case upgradev1alpha1.PhaseCompleted:
		logger.Info("Upgrade completed successfully")
		return ctrl.Result{}, nil
	case upgradev1alpha1.PhaseFailed:
		logger.Info("Upgrade failed")
		return ctrl.Result{}, nil
	default:
		logger.Info("Unknown phase", "phase", upgrade.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
}

// startBackup creates a backup job
func (r *LiqoUpgradeReconciler) startBackup(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting backup phase", "version", upgrade.Spec.CurrentVersion)

	// Create backup job
	job := r.buildBackupJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create backup job")
			return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Failed to create backup job", nil)
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseBackup, "Creating backup of current state", nil)
}

// monitorBackup monitors the backup job
func (r *LiqoUpgradeReconciler) monitorBackup(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", backupJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get backup job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Backup completed successfully")
		// Start CRD upgrade
		return r.startCRDUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Info("Backup failed")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Backup job failed", nil)
	}

	logger.Info("Backup job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// startCRDUpgrade starts the CRD upgrade after backup
func (r *LiqoUpgradeReconciler) startCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting CRD upgrade phase")

	job := r.buildCRDUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create CRD upgrade job")
			return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Failed to create CRD upgrade job", nil)
		}
	}

	statusUpdates := map[string]interface{}{
		"backupReady": true,
		"backupName":  fmt.Sprintf("%s-%s", backupJobPrefix, upgrade.Name),
	}
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "CRD upgrade job created", statusUpdates)
}

// monitorCRDUpgrade monitors the CRD upgrade job
func (r *LiqoUpgradeReconciler) monitorCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", upgradeJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get CRD upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("CRD upgrade completed, starting verification")
		return r.startCRDVerification(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Info("CRD upgrade failed, initiating rollback")
		return r.startRollback(ctx, upgrade, "CRD upgrade job failed")
	}

	logger.Info("CRD upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// startCRDVerification starts health check verification
func (r *LiqoUpgradeReconciler) startCRDVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting CRD verification")

	job := r.buildVerificationJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create verification job")
			return r.startRollback(ctx, upgrade, "Failed to create verification job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseVerifyingCRDs, "Verifying CRD upgrade", nil)
}

// monitorCRDVerification monitors the verification job
func (r *LiqoUpgradeReconciler) monitorCRDVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", verifyJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get verification job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Verification passed, Phase 1 complete!")
		logger.Info("Starting Phase 2: Control Plane Upgrade")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseCRDs,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControlPlane, "Phase 1 complete, starting control plane upgrade", statusUpdates)
	}

	if job.Status.Failed > 0 {
		logger.Info("Verification failed, initiating rollback")
		return r.startRollback(ctx, upgrade, "CRD verification failed")
	}

	logger.Info("Verification job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// startRollback initiates rollback process
func (r *LiqoUpgradeReconciler) startRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting rollback", "reason", reason)

	job := r.buildRollbackJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference for rollback job")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, fmt.Sprintf("Rollback preparation failed: %s", err.Error()), nil)
	}

	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.Info("Rollback job already exists, monitoring it")
			return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseRollingBack, fmt.Sprintf("Rolling back due to: %s", reason), nil)
		}
		logger.Error(err, "Failed to create rollback job")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, fmt.Sprintf("Rollback failed to start: %s | Original failure: %s", err.Error(), reason), nil)
	}

	logger.Info("Rollback job created successfully")
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseRollingBack, fmt.Sprintf("Rolling back due to: %s", reason), nil)
}

// monitorRollback monitors the rollback job
func (r *LiqoUpgradeReconciler) monitorRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get rollback job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Rollback completed successfully")
		statusUpdates := map[string]interface{}{
			"rolledBack": true,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Upgrade failed and rolled back successfully", statusUpdates)
	}

	if job.Status.Failed > 0 {
		logger.Info("Rollback failed!")
		statusUpdates := map[string]interface{}{
			"rolledBack": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Upgrade failed AND rollback failed - manual intervention required", statusUpdates)
	}

	logger.Info("Rollback job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// buildBackupJob creates the backup job
func (r *LiqoUpgradeReconciler) buildBackupJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", backupJobPrefix, upgrade.Name)
	namespace := upgrade.Namespace
	if namespace == "" {
		namespace = "default"
	}

	backupScript := `#!/bin/bash
set -e

BACKUP_DIR="/tmp/backup"
mkdir -p "$BACKUP_DIR"

echo "========================================="
echo "Backing up Liqo CRDs"
echo "========================================="

# Get list of all Liqo CRDs
LIQO_CRDS=$(kubectl get crd | grep liqo | awk '{print $1}')

echo "Found $(echo "$LIQO_CRDS" | wc -l) Liqo CRDs to backup"

# Backup each CRD
for crd in $LIQO_CRDS; do
    echo "Backing up: $crd"
    kubectl get crd "$crd" -o yaml > "$BACKUP_DIR/$crd.yaml"
done

echo ""
echo "✅ Backup completed successfully!"
echo "Backed up $(ls -1 $BACKUP_DIR/*.yaml | wc -l) CRDs"
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300), // Auto-delete 5 minutes after completion
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "backup-crds",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", backupScript},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}

	return job
}

// buildCRDUpgradeJob creates the CRD upgrade job (same as before but enhanced)
func (r *LiqoUpgradeReconciler) buildCRDUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", upgradeJobPrefix, upgrade.Name)
	namespace := upgrade.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Same smart upgrade script as before
	smartUpgradeScript := `#!/bin/bash
set -e

CURRENT_VERSION="` + upgrade.Spec.CurrentVersion + `"
TARGET_VERSION="` + upgrade.Spec.TargetVersion + `"
BASE_URL="https://api.github.com/repos/liqotech/liqo/contents/deployments/liqo/charts/liqo-crds/crds"

echo "========================================="
echo "Smart CRD Upgrade: ${CURRENT_VERSION} → ${TARGET_VERSION}"
echo "========================================="
echo ""

# Step 0: Detect actual version from cluster
echo "Step 0: Validating current version..."

CONTROLLER_IMAGE=$(kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")

if [ -n "$CONTROLLER_IMAGE" ]; then
    ACTUAL_VERSION="${CONTROLLER_IMAGE##*:}"
    
    if ! echo "$ACTUAL_VERSION" | grep -q "^v[0-9]"; then
        ACTUAL_VERSION="unknown"
    fi
else
    ACTUAL_VERSION="unknown"
fi

echo "  User specified: ${CURRENT_VERSION}"
echo "  Cluster has: ${ACTUAL_VERSION}"
echo "  Debug - Full image: ${CONTROLLER_IMAGE}"

if [ -z "$ACTUAL_VERSION" ] || [ "$ACTUAL_VERSION" = "unknown" ]; then
    echo ""
    echo "⚠️  WARNING: Could not detect version from liqo-controller-manager deployment."
    echo "  Proceeding with upgrade, but version validation skipped."
    echo ""
elif [ "$ACTUAL_VERSION" != "$CURRENT_VERSION" ]; then
    echo ""
    echo "❌ ERROR: Version mismatch detected!"
    echo ""
    echo "  You specified currentVersion: ${CURRENT_VERSION}"
    echo "  But cluster actually has: ${ACTUAL_VERSION}"
    echo ""
    echo "  Please update your LiqoUpgrade CR with the correct currentVersion."
    echo ""
    exit 1
fi

echo "  ✅ Version validation passed!"
echo ""

# Function to get list of CRD files from GitHub
get_crd_list() {
    local version=$1
    curl -s "${BASE_URL}?ref=${version}" | grep '"name"' | grep '.yaml' | cut -d'"' -f4
}

# Function to download and hash a CRD
download_and_hash() {
    local version=$1
    local filename=$2
    local output_file=$3
    
    local url="https://raw.githubusercontent.com/liqotech/liqo/${version}/deployments/liqo/charts/liqo-crds/crds/${filename}"
    curl -fsSL "$url" -o "$output_file"
    sha256sum "$output_file" | awk '{print $1}'
}

mkdir -p /tmp/crds/current
mkdir -p /tmp/crds/target
mkdir -p /tmp/crds/changed

echo "Step 1: Fetching CRD lists from GitHub..."
CURRENT_CRDS=$(get_crd_list "$CURRENT_VERSION")
TARGET_CRDS=$(get_crd_list "$TARGET_VERSION")

if [ -z "$CURRENT_CRDS" ]; then
    echo "❌ ERROR: Failed to fetch CRD list for ${CURRENT_VERSION}"
    exit 1
fi

if [ -z "$TARGET_CRDS" ]; then
    echo "❌ ERROR: Failed to fetch CRD list for ${TARGET_VERSION}"
    exit 1
fi

ALL_CRDS=$(echo -e "${CURRENT_CRDS}\n${TARGET_CRDS}" | sort -u)

echo "  Found $(echo "$ALL_CRDS" | wc -l) unique CRDs"
echo ""
echo "Step 2: Comparing CRDs between versions..."

CHANGED_COUNT=0
NEW_COUNT=0
REMOVED_COUNT=0

for crd in $ALL_CRDS; do
    echo "  Checking: $crd"
    
    if echo "$CURRENT_CRDS" | grep -q "^${crd}$"; then
        HAS_CURRENT=true
        CURRENT_HASH=$(download_and_hash "$CURRENT_VERSION" "$crd" "/tmp/crds/current/${crd}")
    else
        HAS_CURRENT=false
        echo "    → NEW in ${TARGET_VERSION}"
        NEW_COUNT=$((NEW_COUNT + 1))
    fi
    
    if echo "$TARGET_CRDS" | grep -q "^${crd}$"; then
        HAS_TARGET=true
        TARGET_HASH=$(download_and_hash "$TARGET_VERSION" "$crd" "/tmp/crds/target/${crd}")
    else
        HAS_TARGET=false
        echo "    → REMOVED in ${TARGET_VERSION}"
        REMOVED_COUNT=$((REMOVED_COUNT + 1))
        continue
    fi
    
    if [ "$HAS_CURRENT" = true ] && [ "$HAS_TARGET" = true ]; then
        if [ "$CURRENT_HASH" != "$TARGET_HASH" ]; then
            echo "    → CHANGED (adding to upgrade list)"
            cp "/tmp/crds/target/${crd}" "/tmp/crds/changed/${crd}"
            CHANGED_COUNT=$((CHANGED_COUNT + 1))
        else
            echo "    → No changes"
        fi
    elif [ "$HAS_TARGET" = true ]; then
        cp "/tmp/crds/target/${crd}" "/tmp/crds/changed/${crd}"
    fi
done

echo ""
echo "========================================="
echo "Comparison Summary:"
echo "  - Changed CRDs: $CHANGED_COUNT"
echo "  - New CRDs: $NEW_COUNT"
echo "  - Removed CRDs: $REMOVED_COUNT"
echo "========================================="

if [ $CHANGED_COUNT -gt 0 ] || [ $NEW_COUNT -gt 0 ]; then
    echo ""
    echo "Step 3: Applying changed/new CRDs..."
    
    for crd_file in /tmp/crds/changed/*.yaml; do
        if [ -f "$crd_file" ]; then
            crd_name=$(basename "$crd_file")
            echo "  Applying: $crd_name"
            
            # Try regular apply first, capture output and exit code
            if kubectl apply -f "$crd_file" > /tmp/apply_output.log 2>&1; then
                echo "    ✓ Applied successfully"
            else
                # Check if failure was due to annotation size
                if grep -q "Too long" /tmp/apply_output.log || grep -q "metadata.annotations" /tmp/apply_output.log; then
                    echo "    → Annotation too large, using server-side apply"
                    kubectl apply -f "$crd_file" --server-side --force-conflicts
                else
                    echo "    ❌ Apply failed with unexpected error:"
                    cat /tmp/apply_output.log
                    exit 1
                fi
            fi
        fi
    done
    
    echo ""
    echo "✅ CRD upgrade completed successfully!"
else
    echo ""
    echo "ℹ️  No CRD changes detected. Nothing to upgrade."
fi

echo ""
echo "Step 4: Verifying CRDs..."
kubectl get crds | grep liqo || true
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300), // Auto-delete 5 minutes after completion
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "upgrade-crds",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", smartUpgradeScript},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}

	return job
}

// buildVerificationJob creates the verification job
func (r *LiqoUpgradeReconciler) buildVerificationJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", verifyJobPrefix, upgrade.Name)
	namespace := upgrade.Namespace
	if namespace == "" {
		namespace = "default"
	}

	verificationScript := `#!/bin/bash
set -e

echo "========================================="
echo "Verifying CRD Upgrade"
echo "========================================="
echo ""

# Step 1: Verify all Liqo CRDs exist
echo "Step 1: Checking all Liqo CRDs exist..."
LIQO_CRDS=$(kubectl get crd | grep liqo | wc -l)
echo "  Found $LIQO_CRDS Liqo CRDs"

if [ "$LIQO_CRDS" -lt 25 ]; then
    echo "  ❌ ERROR: Expected at least 25 Liqo CRDs, found $LIQO_CRDS"
    exit 1
fi

echo "  ✅ CRD count looks good"
echo ""

# Step 2: Verify CRDs are valid (can be retrieved)
echo "Step 2: Verifying CRDs are valid..."
INVALID_CRDS=0

for crd in $(kubectl get crd | grep liqo | awk '{print $1}'); do
    if ! kubectl get crd "$crd" > /dev/null 2>&1; then
        echo "  ❌ ERROR: CRD $crd is invalid"
        INVALID_CRDS=$((INVALID_CRDS + 1))
    fi
done

if [ $INVALID_CRDS -gt 0 ]; then
    echo "  ❌ ERROR: Found $INVALID_CRDS invalid CRDs"
    exit 1
fi

echo "  ✅ All CRDs are valid"
echo ""

# Step 3: Check if existing resources still exist
echo "Step 3: Verifying existing resources..."

# Check ForeignClusters (if any)
FC_COUNT=$(kubectl get foreignclusters.core.liqo.io -A 2>/dev/null | grep -v NAME | wc -l || echo "0")
echo "  Found $FC_COUNT ForeignCluster resources"

# Check if liqo-controller-manager is running
echo ""
echo "Step 4: Verifying Liqo components..."
if kubectl get deployment liqo-controller-manager -n liqo > /dev/null 2>&1; then
    READY=$(kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.status.readyReplicas}')
    if [ "$READY" -ge 1 ]; then
        echo "  ✅ liqo-controller-manager is running"
    else
        echo "  ⚠️  WARNING: liqo-controller-manager is not ready"
    fi
fi

echo ""
echo "========================================="
echo "✅ Verification Passed!"
echo "========================================="
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300), // Auto-delete 5 minutes after completion
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "verify-crds",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", verificationScript},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}

	return job
}

// buildRollbackJob creates the rollback job
func (r *LiqoUpgradeReconciler) buildRollbackJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	namespace := upgrade.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Determine what to rollback based on last successful phase
	var rollbackScript string

	if upgrade.Status.LastSuccessfulPhase == upgradev1alpha1.PhaseCRDs ||
		upgrade.Status.LastSuccessfulPhase == upgradev1alpha1.PhaseVerifyingCRDs {
		// Phase 2 (Control Plane) failed, rollback deployments
		rollbackScript = fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Rolling Back Control Plane"
echo "========================================="
echo ""

NAMESPACE="%s"

echo "Checking for deployment backups..."
if [ ! -f /tmp/controller-manager-backup.yaml ] || [ ! -f /tmp/webhook-backup.yaml ]; then
    echo "❌ No deployment backups found"
    echo "Control plane was not modified, nothing to rollback"
    exit 0
fi

echo "Restoring liqo-controller-manager..."
kubectl apply -f /tmp/controller-manager-backup.yaml
kubectl rollout status deployment/liqo-controller-manager -n "$NAMESPACE" --timeout=3m

echo "Restoring liqo-webhook..."
kubectl apply -f /tmp/webhook-backup.yaml
kubectl rollout status deployment/liqo-webhook -n "$NAMESPACE" --timeout=3m

echo ""
echo "✅ Control plane rollback completed successfully!"
`, namespace)
	} else {
		// Phase 1 (CRD) failed, rollback CRDs
		rollbackScript = `#!/bin/bash
set -e

BACKUP_DIR="/tmp/backup"

echo "========================================="
echo "Rolling Back CRDs"
echo "========================================="
echo ""

if [ ! -d "$BACKUP_DIR" ] || [ -z "$(ls -A $BACKUP_DIR/*.yaml 2>/dev/null)" ]; then
    echo "❌ ERROR: No backup found at $BACKUP_DIR"
    echo "Cannot rollback without backup!"
    exit 1
fi

echo "Found backup with $(ls -1 $BACKUP_DIR/*.yaml | wc -l) CRDs"
echo ""
echo "Restoring CRDs from backup..."

for crd_file in $BACKUP_DIR/*.yaml; do
    crd_name=$(basename "$crd_file" .yaml)
    echo "  Restoring: $crd_name"
    kubectl apply -f "$crd_file" --server-side --force-conflicts
done

echo ""
echo "✅ Rollback completed successfully!"
echo ""
echo "Verifying rollback..."
kubectl get crds | grep liqo || true
`
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "rollback",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", rollbackScript},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}

	return job
}

func (r *LiqoUpgradeReconciler) updateStatus(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, phase upgradev1alpha1.UpgradePhase, message string, additionalUpdates map[string]interface{}) (ctrl.Result, error) {
	upgrade.Status.Phase = phase
	upgrade.Status.Message = message
	upgrade.Status.LastUpdated = metav1.Now()

	// Apply additional status updates
	if additionalUpdates != nil {
		if backupReady, ok := additionalUpdates["backupReady"].(bool); ok {
			upgrade.Status.BackupReady = backupReady
		}
		if backupName, ok := additionalUpdates["backupName"].(string); ok {
			upgrade.Status.BackupName = backupName
		}
		if lastSuccessfulPhase, ok := additionalUpdates["lastSuccessfulPhase"].(upgradev1alpha1.UpgradePhase); ok {
			upgrade.Status.LastSuccessfulPhase = lastSuccessfulPhase
		}
		if rolledBack, ok := additionalUpdates["rolledBack"].(bool); ok {
			upgrade.Status.RolledBack = rolledBack
		}
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) handleDeletion(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(upgrade, finalizerName) {
		// Cleanup: Delete all jobs if they exist
		jobPrefixes := []string{backupJobPrefix, upgradeJobPrefix, verifyJobPrefix, rollbackJobPrefix}
		for _, prefix := range jobPrefixes {
			jobName := fmt.Sprintf("%s-%s", prefix, upgrade.Name)
			job := &batchv1.Job{}
			err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job)
			if err == nil {
				if err := r.Delete(ctx, job); err != nil {
					logger.Error(err, "Failed to delete job", "jobName", jobName)
				}
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(upgrade, finalizerName)
		if err := r.Update(ctx, upgrade); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func int32Ptr(i int32) *int32 {
	return &i
}

// monitorControlPlaneUpgrade monitors the control plane upgrade job
func (r *LiqoUpgradeReconciler) monitorControlPlaneUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("liqo-upgrade-controlplane-%s", upgrade.Name)
	job := &batchv1.Job{}

	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			// Job doesn't exist yet, create it
			logger.Info("Creating control plane upgrade job")
			return r.startControlPlaneUpgradeJob(ctx, upgrade)
		}
		logger.Error(err, "Failed to get control plane upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Control plane upgrade completed successfully!")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseControlPlane,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompleted, "Phase 2 (Control plane upgrade) completed successfully", statusUpdates)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Control plane upgrade failed, initiating rollback")
		return r.startRollback(ctx, upgrade, "Control plane upgrade job failed")
	}

	logger.Info("Control plane upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// startControlPlaneUpgradeJob creates and starts the control plane upgrade job
func (r *LiqoUpgradeReconciler) startControlPlaneUpgradeJob(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting control plane upgrade")

	job := r.buildControlPlaneUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference for control plane upgrade job")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, fmt.Sprintf("Failed to create control plane upgrade job: %s", err.Error()), nil)
	}

	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.Info("Control plane upgrade job already exists, monitoring it")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		logger.Error(err, "Failed to create control plane upgrade job")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, fmt.Sprintf("Failed to start control plane upgrade: %s", err.Error()), nil)
	}

	logger.Info("Control plane upgrade job created successfully")
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// buildControlPlaneUpgradeJob builds the job for upgrading control plane
func (r *LiqoUpgradeReconciler) buildControlPlaneUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("liqo-upgrade-controlplane-%s", upgrade.Name)

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Phase 2: Core Control Plane Upgrade"
echo "========================================="
echo ""

CURRENT_VERSION="%s"
TARGET_VERSION="%s"
NAMESPACE="%s"

echo "Upgrading from $CURRENT_VERSION to $TARGET_VERSION"
echo "Namespace: $NAMESPACE"
echo ""

# Step 1: Backup current deployments
echo "Step 1: Backing up current deployment state..."
kubectl get deployment liqo-controller-manager -n "$NAMESPACE" -o yaml > /tmp/controller-manager-backup.yaml
kubectl get deployment liqo-webhook -n "$NAMESPACE" -o yaml > /tmp/webhook-backup.yaml
echo "  ✓ Backups created"
echo ""

# Step 2: Upgrade controller-manager
echo "Step 2: Upgrading liqo-controller-manager..."
echo "  Current image:"
kubectl get deployment liqo-controller-manager -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""

NEW_CONTROLLER_IMAGE="ghcr.io/liqotech/liqo-controller-manager:${TARGET_VERSION}"
echo "  New image: $NEW_CONTROLLER_IMAGE"

kubectl set image deployment/liqo-controller-manager \
  controller-manager="$NEW_CONTROLLER_IMAGE" \
  -n "$NAMESPACE"

echo "  Waiting for rollout..."
if ! kubectl rollout status deployment/liqo-controller-manager -n "$NAMESPACE" --timeout=5m; then
    echo "  ❌ Controller-manager rollout failed!"
    exit 1
fi

echo "  Verifying controller-manager is healthy..."
if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-controller-manager -n "$NAMESPACE"; then
    echo "  ❌ Controller-manager not healthy!"
    exit 1
fi

echo "  ✓ Controller-manager upgraded successfully"
echo ""

# Step 3: Upgrade webhook
echo "Step 3: Upgrading liqo-webhook..."
echo "  Current image:"
kubectl get deployment liqo-webhook -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}'
echo ""

NEW_WEBHOOK_IMAGE="ghcr.io/liqotech/webhook:${TARGET_VERSION}"
echo "  New image: $NEW_WEBHOOK_IMAGE"

kubectl set image deployment/liqo-webhook \
  webhook="$NEW_WEBHOOK_IMAGE" \
  -n "$NAMESPACE"

echo "  Waiting for rollout..."
if ! kubectl rollout status deployment/liqo-webhook -n "$NAMESPACE" --timeout=5m; then
    echo "  ❌ Webhook rollout failed!"
    exit 1
fi

echo "  Verifying webhook is healthy..."
if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-webhook -n "$NAMESPACE"; then
    echo "  ❌ Webhook not healthy!"
    exit 1
fi

echo "  ✓ Webhook upgraded successfully"
echo ""

# Step 4: Final verification
echo "Step 4: Final verification..."

echo "  Checking controller-manager pods:"
kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=controller-manager

echo ""
echo "  Checking webhook pods:"
kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=webhook

echo ""
echo "  Verifying image versions:"
echo "    Controller-manager: $(kubectl get deployment liqo-controller-manager -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}')"
echo "    Webhook: $(kubectl get deployment liqo-webhook -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}')"

echo ""
echo "✅ Phase 2 (Control Plane) upgrade completed successfully!"
`, upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion, upgrade.Spec.Namespace)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: upgrade.Spec.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "liqo-upgrade",
				"app.kubernetes.io/component":  "controlplane-upgrade",
				"app.kubernetes.io/managed-by": "liqo-upgrade-controller",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: int32Ptr(300),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "upgrade",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.LiqoUpgrade{}).
		Owns(&batchv1.Job{}).
		Named("liqoupgrade").
		Complete(r)
}
