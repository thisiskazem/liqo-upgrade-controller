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
	finalizerName = "upgrade.liqo.io/finalizer"
	jobNamePrefix = "liqo-upgrade-crd"
)

// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/finalizers,verbs=update
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
	switch upgrade.Status.Phase {
	case "":
		return r.startCRDUpgrade(ctx, upgrade)
	case upgradev1alpha1.PhaseCRDs:
		return r.monitorCRDUpgrade(ctx, upgrade)
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

func (r *LiqoUpgradeReconciler) startCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting CRD upgrade phase", "from", upgrade.Spec.CurrentVersion, "to", upgrade.Spec.TargetVersion)

	// Create the Job to upgrade CRDs
	job := r.buildCRDUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create CRD upgrade job")
			return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Failed to create CRD upgrade job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "CRD upgrade job created")
}

func (r *LiqoUpgradeReconciler) monitorCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Job
	jobName := fmt.Sprintf("%s-%s", jobNamePrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get CRD upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Check Job status
	if job.Status.Succeeded > 0 {
		logger.Info("CRD upgrade completed successfully")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompleted, "CRD upgrade completed successfully")
	}

	if job.Status.Failed > 0 {
		logger.Info("CRD upgrade failed")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "CRD upgrade job failed - check job logs for details")
	}

	// Job still running
	logger.Info("CRD upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) buildCRDUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", jobNamePrefix, upgrade.Name)
	namespace := upgrade.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Smart upgrade script with version validation
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

# Try to get version from liqo-controller-manager image tag
CONTROLLER_IMAGE=$(kubectl get deployment liqo-controller-manager -n liqo -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")

if [ -n "$CONTROLLER_IMAGE" ]; then
    # Extract version using parameter expansion (works in any POSIX shell)
    # Remove everything before the last colon
    ACTUAL_VERSION="${CONTROLLER_IMAGE##*:}"
    
    # Verify we got a version-like string (starts with 'v' and contains numbers)
    if ! echo "$ACTUAL_VERSION" | grep -q "^v[0-9]"; then
        ACTUAL_VERSION="unknown"
    fi
else
    ACTUAL_VERSION="unknown"
fi

echo "  User specified: ${CURRENT_VERSION}"
echo "  Cluster has: ${ACTUAL_VERSION}"

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

# Create temp directories
mkdir -p /tmp/crds/current
mkdir -p /tmp/crds/target
mkdir -p /tmp/crds/changed

# Get CRD lists
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

# Merge and deduplicate CRD names
ALL_CRDS=$(echo -e "${CURRENT_CRDS}\n${TARGET_CRDS}" | sort -u)

echo "  Found $(echo "$ALL_CRDS" | wc -l) unique CRDs"
echo ""
echo "Step 2: Comparing CRDs between versions..."

CHANGED_COUNT=0
NEW_COUNT=0
REMOVED_COUNT=0

for crd in $ALL_CRDS; do
    echo "  Checking: $crd"
    
    # Check if CRD exists in current version
    if echo "$CURRENT_CRDS" | grep -q "^${crd}$"; then
        HAS_CURRENT=true
        CURRENT_HASH=$(download_and_hash "$CURRENT_VERSION" "$crd" "/tmp/crds/current/${crd}")
    else
        HAS_CURRENT=false
        echo "    → NEW in ${TARGET_VERSION}"
        NEW_COUNT=$((NEW_COUNT + 1))
    fi
    
    # Check if CRD exists in target version
    if echo "$TARGET_CRDS" | grep -q "^${crd}$"; then
        HAS_TARGET=true
        TARGET_HASH=$(download_and_hash "$TARGET_VERSION" "$crd" "/tmp/crds/target/${crd}")
    else
        HAS_TARGET=false
        echo "    → REMOVED in ${TARGET_VERSION}"
        REMOVED_COUNT=$((REMOVED_COUNT + 1))
        continue
    fi
    
    # Compare hashes if both exist
    if [ "$HAS_CURRENT" = true ] && [ "$HAS_TARGET" = true ]; then
        if [ "$CURRENT_HASH" != "$TARGET_HASH" ]; then
            echo "    → CHANGED (adding to upgrade list)"
            cp "/tmp/crds/target/${crd}" "/tmp/crds/changed/${crd}"
            CHANGED_COUNT=$((CHANGED_COUNT + 1))
        else
            echo "    → No changes"
        fi
    elif [ "$HAS_TARGET" = true ]; then
        # New CRD, add to changed list
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

# Apply only changed CRDs
if [ $CHANGED_COUNT -gt 0 ] || [ $NEW_COUNT -gt 0 ]; then
    echo ""
    echo "Step 3: Applying changed/new CRDs..."
    
    for crd_file in /tmp/crds/changed/*.yaml; do
        if [ -f "$crd_file" ]; then
            crd_name=$(basename "$crd_file")
            echo "  Applying: $crd_name"
            
            # Try regular apply first
            if ! kubectl apply -f "$crd_file" 2>&1 | tee /tmp/apply_output.log | grep -q "Too long"; then
                # Apply succeeded
                echo "    ✓ Applied successfully"
            else
                # Annotation too large, use server-side apply
                echo "    → Annotation too large, using server-side apply"
                kubectl apply -f "$crd_file" --server-side --force-conflicts
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
			BackoffLimit: int32Ptr(0), // No retries - fail immediately on error
		},
	}

	return job
}

func (r *LiqoUpgradeReconciler) updateStatus(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, phase upgradev1alpha1.UpgradePhase, message string) (ctrl.Result, error) {
	upgrade.Status.Phase = phase
	upgrade.Status.Message = message
	upgrade.Status.LastUpdated = metav1.Now()

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) handleDeletion(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(upgrade, finalizerName) {
		// Cleanup: Delete the Job if it exists
		jobName := fmt.Sprintf("%s-%s", jobNamePrefix, upgrade.Name)
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Namespace}, job)
		if err == nil {
			if err := r.Delete(ctx, job); err != nil {
				logger.Error(err, "Failed to delete upgrade job")
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

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.LiqoUpgrade{}).
		Owns(&batchv1.Job{}).
		Named("liqoupgrade").
		Complete(r)
}
