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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"sigs.k8s.io/yaml"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// LiqoUpgradeReconciler reconciles a LiqoUpgrade object
type LiqoUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	finalizerName          = "upgrade.liqo.io/finalizer"
	backupJobPrefix        = "liqo-backup"
	upgradeJobPrefix       = "liqo-upgrade-crd"
	rollbackJobPrefix      = "liqo-rollback"
	controlPlaneJobPrefix  = "liqo-upgrade-controlplane"
	compatibilityConfigMap = "liqo-version-compatibility"
)

// CompatibilityMatrix represents the version compatibility data
type CompatibilityMatrix map[string][]string

// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
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
		logger.Info("Phase is empty, starting compatibility check")
		return r.startCompatibilityCheck(ctx, upgrade)
	case upgradev1alpha1.PhaseCompatibilityCheck:
		return r.checkCompatibility(ctx, upgrade)
	case upgradev1alpha1.PhaseBackup:
		return r.monitorBackup(ctx, upgrade)
	case upgradev1alpha1.PhaseCRDs:
		return r.monitorCRDUpgrade(ctx, upgrade)
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

// startCompatibilityCheck initiates Phase 0: Compatibility Check
func (r *LiqoUpgradeReconciler) startCompatibilityCheck(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Phase 0: Starting compatibility check")

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompatibilityCheck, "Checking version compatibility", nil)
}

// checkCompatibility performs the actual compatibility check logic
func (r *LiqoUpgradeReconciler) checkCompatibility(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step 1: Detect local cluster version
	localVersion, err := r.detectLocalVersion(ctx, upgrade.Spec.Namespace)
	if err != nil {
		logger.Error(err, "Failed to detect local cluster version")
		statusUpdates := map[string]interface{}{
			"compatibilityCheckPassed": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to detect local cluster version: %v", err), statusUpdates)
	}

	logger.Info("Detected local version", "version", localVersion)

	// Step 2: Validate user-specified current version matches detected version
	if upgrade.Spec.CurrentVersion != localVersion {
		logger.Info("Version mismatch detected",
			"specifiedVersion", upgrade.Spec.CurrentVersion,
			"detectedVersion", localVersion)
		statusUpdates := map[string]interface{}{
			"detectedLocalVersion":     localVersion,
			"compatibilityCheckPassed": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Version mismatch: specified %s but detected %s. Please update spec.currentVersion",
				upgrade.Spec.CurrentVersion, localVersion), statusUpdates)
	}

	// Step 3: Determine lowest version among local and remote clusters
	lowestVersion := r.determineLowestVersion(localVersion, upgrade.Spec.RemoteClusterVersions)
	logger.Info("Determined lowest version", "lowestVersion", lowestVersion)

	// Step 4: Load compatibility matrix
	matrix, err := r.loadCompatibilityMatrix(ctx, upgrade.Spec.Namespace)
	if err != nil {
		logger.Error(err, "Failed to load compatibility matrix")
		statusUpdates := map[string]interface{}{
			"compatibilityCheckPassed": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to load compatibility matrix: %v", err), statusUpdates)
	}

	// Step 5: Check if upgrade path is compatible
	compatible := r.isCompatible(matrix, lowestVersion, upgrade.Spec.TargetVersion)

	if !compatible {
		logger.Info("Incompatible upgrade path detected",
			"from", lowestVersion,
			"to", upgrade.Spec.TargetVersion)
		statusUpdates := map[string]interface{}{
			"detectedLocalVersion":     localVersion,
			"lowestVersion":            lowestVersion,
			"compatibilityCheckPassed": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Incompatible upgrade path: %s → %s not supported. Check compatibility matrix.", lowestVersion, upgrade.Spec.TargetVersion),
			statusUpdates)
	}

	// Step 6: Compatibility check passed, proceed to backup phase
	logger.Info("Compatibility check passed!", "from", lowestVersion, "to", upgrade.Spec.TargetVersion)
	statusUpdates := map[string]interface{}{
		"detectedLocalVersion":     localVersion,
		"lowestVersion":            lowestVersion,
		"compatibilityCheckPassed": true,
		"lastSuccessfulPhase":      upgradev1alpha1.PhaseCompatibilityCheck,
	}

	// Update status first
	if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompatibilityCheck,
		fmt.Sprintf("Compatibility check passed: %s → %s", lowestVersion, upgrade.Spec.TargetVersion), statusUpdates); err != nil {
		return ctrl.Result{}, err
	}

	// Now start backup
	return r.startBackup(ctx, upgrade)
}

// detectLocalVersion detects the Liqo version from liqo-controller-manager deployment
func (r *LiqoUpgradeReconciler) detectLocalVersion(ctx context.Context, namespace string) (string, error) {
	// TODO(future): Implement auto-detection from ForeignCluster CRs
	// For now, we detect only from the local liqo-controller-manager deployment

	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)

	if err != nil {
		return "", fmt.Errorf("failed to get liqo-controller-manager deployment: %w", err)
	}

	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in liqo-controller-manager deployment")
	}

	image := deployment.Spec.Template.Spec.Containers[0].Image
	parts := strings.Split(image, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("could not parse version from image: %s", image)
	}

	version := parts[len(parts)-1]
	if !strings.HasPrefix(version, "v") {
		return "", fmt.Errorf("invalid version format: %s (expected vX.Y.Z)", version)
	}

	return version, nil
}

// determineLowestVersion finds the lowest version among local and remote clusters
func (r *LiqoUpgradeReconciler) determineLowestVersion(localVersion string, remoteVersions []upgradev1alpha1.RemoteClusterVersion) string {
	versions := []string{localVersion}

	if len(remoteVersions) == 0 {
		// Conservative approach: if no remote versions specified, assume they're same as local
		return localVersion
	}

	for _, remote := range remoteVersions {
		versions = append(versions, remote.Version)
	}

	// Simple version comparison (works for semver vX.Y.Z)
	// TODO: Use proper semver library for production
	lowest := versions[0]
	for _, v := range versions[1:] {
		if compareVersions(v, lowest) < 0 {
			lowest = v
		}
	}

	return lowest
}

// compareVersions compares two semantic versions (simple implementation)
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func compareVersions(v1, v2 string) int {
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	for i := 0; i < len(parts1) && i < len(parts2); i++ {
		var n1, n2 int
		fmt.Sscanf(parts1[i], "%d", &n1)
		fmt.Sscanf(parts2[i], "%d", &n2)

		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	if len(parts1) < len(parts2) {
		return -1
	}
	if len(parts1) > len(parts2) {
		return 1
	}

	return 0
}

// loadCompatibilityMatrix loads the compatibility matrix from ConfigMap
func (r *LiqoUpgradeReconciler) loadCompatibilityMatrix(ctx context.Context, namespace string) (CompatibilityMatrix, error) {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      compatibilityConfigMap,
		Namespace: namespace,
	}, configMap)

	if err != nil {
		return nil, fmt.Errorf("failed to get compatibility ConfigMap: %w", err)
	}

	yamlData, ok := configMap.Data["compatibility.yaml"]
	if !ok {
		return nil, fmt.Errorf("compatibility.yaml not found in ConfigMap")
	}

	var matrix CompatibilityMatrix
	if err := yaml.Unmarshal([]byte(yamlData), &matrix); err != nil {
		return nil, fmt.Errorf("failed to parse compatibility matrix: %w", err)
	}

	return matrix, nil
}

// isCompatible checks if upgrading from sourceVersion to targetVersion is supported
func (r *LiqoUpgradeReconciler) isCompatible(matrix CompatibilityMatrix, sourceVersion, targetVersion string) bool {
	compatibleVersions, exists := matrix[sourceVersion]
	if !exists {
		return false
	}

	for _, compatible := range compatibleVersions {
		if compatible == targetVersion {
			return true
		}
	}

	return false
}

// startBackup creates a backup job (Phase 1)
func (r *LiqoUpgradeReconciler) startBackup(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Phase 1: Starting backup phase", "version", upgrade.Spec.CurrentVersion)

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
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get backup job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Backup completed successfully")
		statusUpdates := map[string]interface{}{
			"backupReady":         true,
			"backupName":          jobName,
			"lastSuccessfulPhase": upgradev1alpha1.PhaseBackup,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseBackup, "Backup completed", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
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

// startCRDUpgrade starts Phase 2: CRD upgrade (includes verification)
func (r *LiqoUpgradeReconciler) startCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Phase 2: Starting CRD upgrade with verification")

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

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "CRD upgrade job created", nil)
}

// monitorCRDUpgrade monitors the CRD upgrade job (Phase 2)
func (r *LiqoUpgradeReconciler) monitorCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", upgradeJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get CRD upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Phase 2 completed: CRD upgrade and verification successful")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseCRDs,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "CRD upgrade and verification completed", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
		// Start control plane upgrade
		return r.startControlPlaneUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Info("Phase 2 failed: CRD upgrade or verification failed, initiating rollback")
		return r.startRollback(ctx, upgrade, "Phase 2 (CRD upgrade/verification) failed")
	}

	logger.Info("CRD upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// startControlPlaneUpgrade starts Phase 3: Control Plane upgrade (includes verification)
func (r *LiqoUpgradeReconciler) startControlPlaneUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Phase 3: Starting control plane upgrade with verification")

	job := r.buildControlPlaneUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create control plane upgrade job")
			return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Failed to create control plane upgrade job", nil)
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControlPlane, "Control plane upgrade job created", nil)
}

// monitorControlPlaneUpgrade monitors the control plane upgrade job (Phase 3)
func (r *LiqoUpgradeReconciler) monitorControlPlaneUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", controlPlaneJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get control plane upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Phase 3 completed: Control plane upgrade and verification successful!")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseControlPlane,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControlPlane, "Control plane upgrade and verification completed", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
		// Proceed to cleanup and completion
		return r.completeUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Phase 3 failed: Control plane upgrade or verification failed, rolling back EVERYTHING")
		return r.startRollback(ctx, upgrade, "Phase 3 (Control plane upgrade/verification) failed - rolling back everything")
	}

	logger.Info("Control plane upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// completeUpgrade marks the upgrade as completed and cleans up backup (Phase 4)
func (r *LiqoUpgradeReconciler) completeUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Phase 4: Completing upgrade and cleaning up")

	// Delete backup job
	backupJobName := fmt.Sprintf("%s-%s", backupJobPrefix, upgrade.Name)
	backupJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: backupJobName, Namespace: upgrade.Spec.Namespace}, backupJob); err == nil {
		logger.Info("Deleting backup job", "jobName", backupJobName)
		if err := r.Delete(ctx, backupJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			logger.Error(err, "Failed to delete backup job, continuing anyway")
		}
	}

	statusUpdates := map[string]interface{}{
		"backupReady": false, // Backup is deleted
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompleted,
		fmt.Sprintf("Upgrade completed successfully: %s → %s", upgrade.Status.LowestVersion, upgrade.Spec.TargetVersion),
		statusUpdates)
}

// startRollback initiates rollback process
func (r *LiqoUpgradeReconciler) startRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting rollback", "reason", reason, "lastSuccessfulPhase", upgrade.Status.LastSuccessfulPhase)

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
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
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

// buildBackupJob creates the backup job (NO TTL - persists until Phase 4)
func (r *LiqoUpgradeReconciler) buildBackupJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", backupJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	backupScript := `#!/bin/bash
set -e

BACKUP_DIR="/tmp/backup"
mkdir -p "$BACKUP_DIR"

echo "========================================="
echo "Phase 1: Backing up Liqo CRDs and Control Plane"
echo "========================================="

# Backup CRDs
echo "Backing up CRDs..."
LIQO_CRDS=$(kubectl get crd | grep liqo | awk '{print $1}')
echo "Found $(echo "$LIQO_CRDS" | wc -l) Liqo CRDs to backup"

for crd in $LIQO_CRDS; do
    echo "  Backing up: $crd"
    kubectl get crd "$crd" -o yaml > "$BACKUP_DIR/$crd.yaml"
done

# Backup Control Plane deployments
echo ""
echo "Backing up control plane deployments..."
kubectl get deployment liqo-controller-manager -n ` + namespace + ` -o yaml > "$BACKUP_DIR/controller-manager-backup.yaml"
kubectl get deployment liqo-webhook -n ` + namespace + ` -o yaml > "$BACKUP_DIR/webhook-backup.yaml"

echo ""
echo "✅ Backup completed successfully!"
echo "Backed up:"
echo "  - $(ls -1 $BACKUP_DIR/*.yaml | grep crd | wc -l) CRDs"
echo "  - 2 Control plane deployments"
echo ""
echo "Backup persisted in pod - will be deleted in Phase 4 (Completed)"
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "backup",
			},
		},
		Spec: batchv1.JobSpec{
			// NO TTL - pod persists until Phase 4
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      "liqo-upgrade",
						"app.kubernetes.io/component": "backup",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "liqo-upgrade-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "backup",
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

// buildCRDUpgradeJob creates the CRD upgrade job with embedded verification
func (r *LiqoUpgradeReconciler) buildCRDUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", upgradeJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	// Script includes CRD upgrade + verification in one job
	script := `#!/bin/bash
set -e

CURRENT_VERSION="` + upgrade.Spec.CurrentVersion + `"
TARGET_VERSION="` + upgrade.Spec.TargetVersion + `"
BASE_URL="https://api.github.com/repos/liqotech/liqo/contents/deployments/liqo/charts/liqo-crds/crds"

echo "========================================="
echo "Phase 2: CRD Upgrade + Verification"
echo "========================================="
echo "Upgrading: ${CURRENT_VERSION} → ${TARGET_VERSION}"
echo ""

# Step 1: Version validation (already done in Phase 0, but double-check)
echo "Step 1: Validating current version..."
CONTROLLER_IMAGE=$(kubectl get deployment liqo-controller-manager -n ` + namespace + ` -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")

if [ -n "$CONTROLLER_IMAGE" ]; then
    ACTUAL_VERSION="${CONTROLLER_IMAGE##*:}"
    if ! echo "$ACTUAL_VERSION" | grep -q "^v[0-9]"; then
        ACTUAL_VERSION="unknown"
    fi
else
    ACTUAL_VERSION="unknown"
fi

echo "  Current version: ${ACTUAL_VERSION}"
if [ "$ACTUAL_VERSION" != "$CURRENT_VERSION" ]; then
    echo "❌ ERROR: Version mismatch!"
    exit 1
fi
echo "  ✅ Version validation passed"
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

echo "Step 2: Fetching CRD lists from GitHub..."
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

echo "Step 3: Comparing CRDs between versions..."
CHANGED_COUNT=0
NEW_COUNT=0

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
echo "Summary: Changed=$CHANGED_COUNT, New=$NEW_COUNT"
echo ""

if [ $CHANGED_COUNT -gt 0 ] || [ $NEW_COUNT -gt 0 ]; then
    echo "Step 4: Applying changed/new CRDs..."
    
    for crd_file in /tmp/crds/changed/*.yaml; do
        if [ -f "$crd_file" ]; then
            crd_name=$(basename "$crd_file")
            echo "  Applying: $crd_name"
            
            if kubectl apply -f "$crd_file" > /tmp/apply_output.log 2>&1; then
                echo "    ✓ Applied successfully"
            else
                if grep -q "Too long" /tmp/apply_output.log || grep -q "metadata.annotations" /tmp/apply_output.log; then
                    echo "    → Annotation too large, using server-side apply"
                    kubectl apply -f "$crd_file" --server-side --force-conflicts
                else
                    echo "    ❌ Apply failed:"
                    cat /tmp/apply_output.log
                    exit 1
                fi
            fi
        fi
    done
    echo "  ✅ CRD upgrade completed"
else
    echo "ℹ️  No CRD changes detected."
fi

echo ""
echo "Step 5: Verifying CRDs..."
LIQO_CRDS=$(kubectl get crd | grep liqo | wc -l)
echo "  Found $LIQO_CRDS Liqo CRDs"

if [ "$LIQO_CRDS" -lt 25 ]; then
    echo "  ❌ ERROR: Expected at least 25 Liqo CRDs, found $LIQO_CRDS"
    exit 1
fi

# Verify CRDs are valid
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

# Verify liqo-controller-manager is still running
if kubectl get deployment liqo-controller-manager -n ` + namespace + ` > /dev/null 2>&1; then
    READY=$(kubectl get deployment liqo-controller-manager -n ` + namespace + ` -o jsonpath='{.status.readyReplicas}')
    if [ "$READY" -ge 1 ]; then
        echo "  ✅ liqo-controller-manager is running"
    else
        echo "  ⚠️  WARNING: liqo-controller-manager is not ready"
    fi
fi

echo ""
echo "========================================="
echo "✅ Phase 2 Complete: CRD Upgrade + Verification Passed!"
echo "========================================="
`

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "crd-upgrade",
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
							Name:    "upgrade-crds",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}

	return job
}

// buildControlPlaneUpgradeJob creates the control plane upgrade job with embedded verification
func (r *LiqoUpgradeReconciler) buildControlPlaneUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", controlPlaneJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Phase 3: Control Plane Upgrade + Verification"
echo "========================================="

CURRENT_VERSION="%s"
TARGET_VERSION="%s"
NAMESPACE="%s"

echo "Upgrading from $CURRENT_VERSION to $TARGET_VERSION"
echo "Namespace: $NAMESPACE"
echo ""

# Step 1: Upgrade controller-manager
echo "Step 1: Upgrading liqo-controller-manager..."
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

echo "  ✅ Controller-manager upgraded and verified"
echo ""

# Step 2: Upgrade webhook
echo "Step 2: Upgrading liqo-webhook..."
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

echo "  ✅ Webhook upgraded and verified"
echo ""

# Step 3: Final verification
echo "Step 3: Final verification..."
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
echo "========================================="
echo "✅ Phase 3 Complete: Control Plane Upgrade + Verification Passed!"
echo "========================================="
`, upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion, namespace)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "controlplane-upgrade",
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
							Name:    "upgrade-controlplane",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}

// buildRollbackJob creates the rollback job
// Logic: Phase 2 failed → rollback CRDs only
//
//	Phase 3 failed → rollback EVERYTHING (CRDs + Control Plane)
func (r *LiqoUpgradeReconciler) buildRollbackJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	var rollbackScript string

	if upgrade.Status.LastSuccessfulPhase == upgradev1alpha1.PhaseControlPlane {
		// Phase 3 failed - rollback EVERYTHING
		rollbackScript = `#!/bin/bash
set -e

echo "========================================="
echo "Rolling Back EVERYTHING (CRDs + Control Plane)"
echo "========================================="
echo ""

BACKUP_POD=$(kubectl get pods -n ` + namespace + ` -l app.kubernetes.io/component=backup -o jsonpath='{.items[0].metadata.name}')

if [ -z "$BACKUP_POD" ]; then
    echo "❌ ERROR: Backup pod not found!"
    exit 1
fi

echo "Found backup pod: $BACKUP_POD"
echo ""

# Step 1: Rollback control plane
echo "Step 1: Rolling back control plane..."
kubectl cp ` + namespace + `/$BACKUP_POD:/tmp/backup/controller-manager-backup.yaml /tmp/controller-manager-backup.yaml
kubectl cp ` + namespace + `/$BACKUP_POD:/tmp/backup/webhook-backup.yaml /tmp/webhook-backup.yaml

echo "  Restoring liqo-controller-manager..."
kubectl apply -f /tmp/controller-manager-backup.yaml
kubectl rollout status deployment/liqo-controller-manager -n ` + namespace + ` --timeout=3m

echo "  Restoring liqo-webhook..."
kubectl apply -f /tmp/webhook-backup.yaml
kubectl rollout status deployment/liqo-webhook -n ` + namespace + ` --timeout=3m

echo "  ✅ Control plane rolled back"
echo ""

# Step 2: Rollback CRDs
echo "Step 2: Rolling back CRDs..."
LIQO_CRDS=$(kubectl exec -n ` + namespace + ` $BACKUP_POD -- ls /tmp/backup/*.yaml | grep -v "controller-manager\|webhook" | wc -l)
echo "  Found $LIQO_CRDS CRDs to restore"

kubectl exec -n ` + namespace + ` $BACKUP_POD -- ls /tmp/backup/*.yaml | grep -v "controller-manager\|webhook" | while read crd_file; do
    crd_name=$(basename "$crd_file")
    echo "    Restoring: $crd_name"
    kubectl exec -n ` + namespace + ` $BACKUP_POD -- cat "$crd_file" | kubectl apply --server-side --force-conflicts -f -
done

echo "  ✅ CRDs rolled back"
echo ""
echo "✅ Complete rollback successful!"
`
	} else {
		// Phase 2 failed - rollback CRDs only
		rollbackScript = `#!/bin/bash
set -e

echo "========================================="
echo "Rolling Back CRDs Only"
echo "========================================="
echo ""

BACKUP_POD=$(kubectl get pods -n ` + namespace + ` -l app.kubernetes.io/component=backup -o jsonpath='{.items[0].metadata.name}')

if [ -z "$BACKUP_POD" ]; then
    echo "❌ ERROR: Backup pod not found!"
    exit 1
fi

echo "Found backup pod: $BACKUP_POD"
echo ""

LIQO_CRDS=$(kubectl exec -n ` + namespace + ` $BACKUP_POD -- ls /tmp/backup/*.yaml | grep -v "controller-manager\|webhook" | wc -l)
echo "Found $LIQO_CRDS CRDs to restore"
echo ""

kubectl exec -n ` + namespace + ` $BACKUP_POD -- ls /tmp/backup/*.yaml | grep -v "controller-manager\|webhook" | while read crd_file; do
    crd_name=$(basename "$crd_file")
    echo "  Restoring: $crd_name"
    kubectl exec -n ` + namespace + ` $BACKUP_POD -- cat "$crd_file" | kubectl apply --server-side --force-conflicts -f -
done

echo ""
echo "✅ CRD rollback successful!"
`
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "rollback",
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
							Name:    "rollback",
							Image:   "bitnami/kubectl:latest",
							Command: []string{"/bin/bash", "-c", rollbackScript},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
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
		if detectedLocalVersion, ok := additionalUpdates["detectedLocalVersion"].(string); ok {
			upgrade.Status.DetectedLocalVersion = detectedLocalVersion
		}
		if lowestVersion, ok := additionalUpdates["lowestVersion"].(string); ok {
			upgrade.Status.LowestVersion = lowestVersion
		}
		if compatibilityCheckPassed, ok := additionalUpdates["compatibilityCheckPassed"].(bool); ok {
			upgrade.Status.CompatibilityCheckPassed = &compatibilityCheckPassed
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
		jobPrefixes := []string{backupJobPrefix, upgradeJobPrefix, rollbackJobPrefix, controlPlaneJobPrefix}
		for _, prefix := range jobPrefixes {
			jobName := fmt.Sprintf("%s-%s", prefix, upgrade.Name)
			job := &batchv1.Job{}
			err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job)
			if err == nil {
				if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
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

// SetupWithManager sets up the controller with the Manager.
func (r *LiqoUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.LiqoUpgrade{}).
		Owns(&batchv1.Job{}).
		Named("liqoupgrade").
		Complete(r)
}
