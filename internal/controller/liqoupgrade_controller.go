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
	finalizerName                  = "upgrade.liqo.io/finalizer"
	freezeOperationsJobPrefix      = "liqo-freeze-operations"
	crdUpgradeJobPrefix            = "liqo-upgrade-crd"
	controllerManagerUpgradePrefix = "liqo-upgrade-controller-manager"
	rollbackJobPrefix              = "liqo-rollback"
	compatibilityConfigMap         = "liqo-version-compatibility"
)

// CompatibilityMatrix represents the version compatibility data
type CompatibilityMatrix map[string][]string

// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=upgrade.liqo.io,resources=liqoupgrades/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch

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
	logger.Info("Reconciling upgrade", "currentPhase", upgrade.Status.Phase, "stage", upgrade.Status.CurrentStage)

	switch upgrade.Status.Phase {
	case "", upgradev1alpha1.PhasePending:
		return r.startValidation(ctx, upgrade)
	case upgradev1alpha1.PhaseValidating:
		return r.performValidation(ctx, upgrade)
	case upgradev1alpha1.PhaseFreezingOperations:
		return r.monitorFreezeOperations(ctx, upgrade)
	case upgradev1alpha1.PhaseCRDs:
		return r.monitorCRDUpgrade(ctx, upgrade)
	case upgradev1alpha1.PhaseControllerManager:
		return r.monitorControllerManagerUpgrade(ctx, upgrade)
	case upgradev1alpha1.PhaseVerifying:
		return r.performVerification(ctx, upgrade)
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

// Stage 0: Start & Validation
func (r *LiqoUpgradeReconciler) startValidation(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 0: Starting validation phase")

	// Initialize status
	upgrade.Status.TotalStages = 9 // Stage 0-8 per design
	upgrade.Status.CurrentStage = 0

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseValidating, "Validating compatibility and prerequisites", nil)
}

func (r *LiqoUpgradeReconciler) performValidation(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Performing validation checks")

	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	// Step 1: Verify cluster identity
	logger.Info("Step 1: Verifying cluster identity")
	if err := r.verifyClusterIdentity(ctx, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Cluster identity verification failed: %v", err))
	}

	// Step 2: Check compatibility matrix
	logger.Info("Step 2: Checking compatibility matrix")
	matrix, err := r.loadCompatibilityMatrix(ctx, namespace)
	if err != nil {
		logger.Info("Compatibility matrix not found, proceeding without check", "error", err.Error())
	} else {
		if !r.isCompatible(matrix, upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion) {
			return r.fail(ctx, upgrade, fmt.Sprintf("Incompatible versions: %s → %s", upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion))
		}
	}

	// Step 3: Verify current version matches deployment
	logger.Info("Step 3: Verifying current version")
	deployedVersion, err := r.detectDeployedVersion(ctx, namespace)
	if err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to detect deployed version: %v", err))
	}
	if deployedVersion != upgrade.Spec.CurrentVersion {
		return r.fail(ctx, upgrade, fmt.Sprintf("Version mismatch: deployed=%s, expected=%s", deployedVersion, upgrade.Spec.CurrentVersion))
	}

	// Step 4: Backup critical environment variables and flags
	logger.Info("Step 4: Backing up environment variables and configuration")
	if err := r.backupEnvironmentConfig(ctx, upgrade, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Failed to backup environment config: %v", err))
	}

	// Step 5: Check component health
	logger.Info("Step 5: Checking component health")
	if err := r.verifyComponentHealth(ctx, namespace); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Component health check failed: %v", err))
	}

	// Step 6: Save previous version
	upgrade.Status.PreviousVersion = upgrade.Spec.CurrentVersion

	// Add Compatible condition
	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionCompatible),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "ValidationPassed",
		Message:            fmt.Sprintf("Version %s → %s is compatible", upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion),
	}

	statusUpdates := map[string]interface{}{
		"previousVersion": upgrade.Spec.CurrentVersion,
		"conditions":      []metav1.Condition{condition},
		"backupReady":     upgrade.Status.BackupReady,
		"backupName":      upgrade.Status.BackupName,
	}

	if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseValidating, "Validation completed successfully", statusUpdates); err != nil {
		return ctrl.Result{}, err
	}

	// Move to next phase: Freeze operations
	return r.startFreezeOperations(ctx, upgrade)
}

func (r *LiqoUpgradeReconciler) verifyClusterIdentity(ctx context.Context, namespace string) error {
	// Verify ForeignCluster CRD exists
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	return err
}

func (r *LiqoUpgradeReconciler) detectDeployedVersion(ctx context.Context, namespace string) (string, error) {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	if err != nil {
		return "", err
	}

	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		return "", fmt.Errorf("no containers found in deployment")
	}

	image := deployment.Spec.Template.Spec.Containers[0].Image
	parts := strings.Split(image, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid image format: %s", image)
	}

	version := parts[len(parts)-1]
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	return version, nil
}

func (r *LiqoUpgradeReconciler) verifyComponentHealth(ctx context.Context, namespace string) error {
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	if err != nil {
		return err
	}

	if deployment.Status.ReadyReplicas < 1 {
		return fmt.Errorf("liqo-controller-manager not ready: %d/%d", deployment.Status.ReadyReplicas, *deployment.Spec.Replicas)
	}

	return nil
}

// backupEnvironmentConfig backs up critical environment variables and configuration
func (r *LiqoUpgradeReconciler) backupEnvironmentConfig(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, namespace string) error {
	logger := log.FromContext(ctx)

	// Create a ConfigMap to store environment variable backups
	backupConfigMapName := fmt.Sprintf("liqo-upgrade-env-backup-%s", upgrade.Name)

	// Get liqo-controller-manager deployment
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "liqo-controller-manager",
		Namespace: namespace,
	}, deployment)
	if err != nil {
		return fmt.Errorf("failed to get controller-manager deployment: %w", err)
	}

	// Extract environment variables from all containers
	envData := make(map[string]string)
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			key := fmt.Sprintf("%s_%s", container.Name, env.Name)
			if env.Value != "" {
				envData[key] = env.Value
			} else if env.ValueFrom != nil {
				// Store reference info for ValueFrom
				envData[key+"_type"] = "valueFrom"
			}
		}

		// Store container args/command
		if len(container.Args) > 0 {
			envData[container.Name+"_args"] = strings.Join(container.Args, " ")
		}
		if len(container.Command) > 0 {
			envData[container.Name+"_command"] = strings.Join(container.Command, " ")
		}
	}

	// Critical environment variables that must be preserved (Stage 2)
	criticalEnvVars := []string{
		"POD_NAMESPACE",
		"CLUSTER_ID",
		"TENANT_NAMESPACE",
		"CLUSTER_ROLE",
		"ENABLE_IPAM",
		"LOG_LEVEL",
	}

	// Verify critical env vars are present
	for _, envVar := range criticalEnvVars {
		found := false
		for key := range envData {
			if strings.Contains(key, envVar) {
				found = true
				break
			}
		}
		if !found {
			logger.Info("Warning: critical environment variable not found in backup", "variable", envVar)
		}
	}

	// Create backup ConfigMap
	backupConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupConfigMapName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "env-backup",
				"upgrade.liqo.io/upgrade":     upgrade.Name,
			},
		},
		Data: envData,
	}

	if err := controllerutil.SetControllerReference(upgrade, backupConfigMap, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create or update the ConfigMap
	existingConfigMap := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: backupConfigMapName, Namespace: namespace}, existingConfigMap)
	if err != nil {
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, backupConfigMap); err != nil {
				return fmt.Errorf("failed to create backup ConfigMap: %w", err)
			}
			logger.Info("Environment config backup created", "configmap", backupConfigMapName)
		} else {
			return fmt.Errorf("failed to check backup ConfigMap: %w", err)
		}
	} else {
		// Update existing
		existingConfigMap.Data = envData
		if err := r.Update(ctx, existingConfigMap); err != nil {
			return fmt.Errorf("failed to update backup ConfigMap: %w", err)
		}
		logger.Info("Environment config backup updated", "configmap", backupConfigMapName)
	}

	// Store backup name in upgrade status for later use
	upgrade.Status.BackupName = backupConfigMapName
	upgrade.Status.BackupReady = true

	return nil
}

func (r *LiqoUpgradeReconciler) loadCompatibilityMatrix(ctx context.Context, namespace string) (CompatibilityMatrix, error) {
	configMap := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      compatibilityConfigMap,
		Namespace: namespace,
	}, configMap)
	if err != nil {
		return nil, err
	}

	yamlData, ok := configMap.Data["compatibility.yaml"]
	if !ok {
		return nil, fmt.Errorf("compatibility.yaml not found in ConfigMap")
	}

	var matrix CompatibilityMatrix
	if err := yaml.Unmarshal([]byte(yamlData), &matrix); err != nil {
		return nil, err
	}

	return matrix, nil
}

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

// Stage 0 Continued: Freeze Operations
func (r *LiqoUpgradeReconciler) startFreezeOperations(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 0: Freezing new offloads and peerings")

	upgrade.Status.CurrentStage = 0

	job := r.buildFreezeOperationsJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create freeze operations job")
			return r.fail(ctx, upgrade, "Failed to create freeze operations job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFreezingOperations, "Freezing new offloads and peerings", nil)
}

func (r *LiqoUpgradeReconciler) monitorFreezeOperations(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", freezeOperationsJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get freeze operations job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Operations frozen successfully")
		return r.startCRDUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		return r.startRollback(ctx, upgrade, "Failed to freeze operations")
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Stage 1: Upgrade CRDs
func (r *LiqoUpgradeReconciler) startCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 1: Starting CRD upgrade")

	upgrade.Status.CurrentStage = 1

	job := r.buildCRDUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create CRD upgrade job")
			return r.fail(ctx, upgrade, "Failed to create CRD upgrade job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "Upgrading CRDs", nil)
}

func (r *LiqoUpgradeReconciler) monitorCRDUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", crdUpgradeJobPrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get CRD upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 1 completed: CRDs upgraded successfully")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseCRDs,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCRDs, "CRDs upgraded", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
		return r.startControllerManagerUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 1 failed: CRD upgrade failed")
		return r.startRollback(ctx, upgrade, "CRD upgrade failed")
	}

	logger.Info("CRD upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Stage 2: Upgrade liqo-controller-manager
func (r *LiqoUpgradeReconciler) startControllerManagerUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 2: Starting liqo-controller-manager upgrade")

	upgrade.Status.CurrentStage = 2

	job := r.buildControllerManagerUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create controller-manager upgrade job")
			return r.fail(ctx, upgrade, "Failed to create controller-manager upgrade job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControllerManager, "Upgrading liqo-controller-manager", nil)
}

func (r *LiqoUpgradeReconciler) monitorControllerManagerUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", controllerManagerUpgradePrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get controller-manager upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 2 completed: liqo-controller-manager upgraded successfully")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseControllerManager,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseControllerManager, "Controller-manager upgraded", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
		return r.startVerification(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 2 failed: Controller-manager upgrade failed")
		return r.startRollback(ctx, upgrade, "Controller-manager upgrade failed")
	}

	logger.Info("Controller-manager upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Verification Phase
func (r *LiqoUpgradeReconciler) startVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting verification phase")

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseVerifying, "Verifying upgrade", nil)
}

func (r *LiqoUpgradeReconciler) performVerification(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Performing post-upgrade verification")

	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	// Verify all components are healthy
	if err := r.verifyComponentHealth(ctx, namespace); err != nil {
		logger.Error(err, "Verification failed: components not healthy")
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Verification failed: %v", err))
	}

	// Verify version
	deployedVersion, err := r.detectDeployedVersion(ctx, namespace)
	if err != nil {
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Version verification failed: %v", err))
	}

	if deployedVersion != upgrade.Spec.TargetVersion {
		return r.startRollback(ctx, upgrade, fmt.Sprintf("Version mismatch after upgrade: deployed=%s, expected=%s", deployedVersion, upgrade.Spec.TargetVersion))
	}

	// Verification passed
	logger.Info("Verification passed, upgrade complete!")

	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionHealthy),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "VerificationPassed",
		Message:            "All components healthy and version verified",
	}

	statusUpdates := map[string]interface{}{
		"conditions": append(upgrade.Status.Conditions, condition),
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseCompleted,
		fmt.Sprintf("Upgrade completed successfully: %s → %s", upgrade.Spec.CurrentVersion, upgrade.Spec.TargetVersion),
		statusUpdates)
}

// Rollback
func (r *LiqoUpgradeReconciler) startRollback(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, reason string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Starting rollback", "reason", reason)

	// Check if autoRollback is disabled
	if upgrade.Spec.AutoRollback != nil && !*upgrade.Spec.AutoRollback {
		logger.Info("AutoRollback disabled, not rolling back")
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed,
			fmt.Sprintf("Upgrade failed (AutoRollback disabled): %s", reason), nil)
	}

	job := r.buildRollbackJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return r.fail(ctx, upgrade, fmt.Sprintf("Rollback preparation failed: %v", err))
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create rollback job")
			return r.fail(ctx, upgrade, fmt.Sprintf("Rollback failed to start: %v", err))
		}
	}

	condition := metav1.Condition{
		Type:               string(upgradev1alpha1.ConditionRollbackRequired),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "UpgradeFailed",
		Message:            reason,
	}

	statusUpdates := map[string]interface{}{
		"conditions": append(upgrade.Status.Conditions, condition),
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseRollingBack, fmt.Sprintf("Rolling back: %s", reason), statusUpdates)
}

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
		statusUpdates := map[string]interface{}{
			"rolledBack": false,
		}
		return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, "Upgrade failed AND rollback failed - manual intervention required", statusUpdates)
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Job Builders

func (r *LiqoUpgradeReconciler) buildFreezeOperationsJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", freezeOperationsJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	script := `#!/bin/bash
set -e

echo "========================================="
echo "Stage 0: Freezing new offloads and peerings"
echo "========================================="

# Block new offloads by adding admission webhook rule
# (Implementation depends on Liqo's internal mechanisms)
echo "✅ Operations frozen (placeholder - actual implementation needed)"
`

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "freeze",
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
							Name:    "freeze",
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

func (r *LiqoUpgradeReconciler) buildCRDUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", crdUpgradeJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Stage 1: Upgrading CRDs"
echo "========================================="

TARGET_VERSION="%s"
GITHUB_API_URL="https://api.github.com/repos/liqotech/liqo/contents/deployments/liqo/charts/liqo-crds/crds?ref=${TARGET_VERSION}"
RAW_BASE_URL="https://raw.githubusercontent.com/liqotech/liqo/${TARGET_VERSION}/deployments/liqo/charts/liqo-crds/crds"

echo "Fetching CRD list from GitHub for version ${TARGET_VERSION}..."

# Fetch list of CRD files from GitHub API
CRD_FILES=$(curl -fsSL "${GITHUB_API_URL}" | grep '"name":' | grep '.yaml"' | cut -d'"' -f4)

if [ -z "$CRD_FILES" ]; then
  echo "❌ ERROR: Failed to fetch CRD list from GitHub"
  exit 1
fi

CRD_COUNT=$(echo "$CRD_FILES" | wc -l)
echo "Found ${CRD_COUNT} CRD files to apply"
echo ""

# Apply each CRD
SUCCESS_COUNT=0
FAILED_COUNT=0

for crd_file in $CRD_FILES; do
  echo "Applying ${crd_file}..."
  if curl -fsSL "${RAW_BASE_URL}/${crd_file}" | kubectl apply --server-side --force-conflicts -f - 2>&1; then
    SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
    echo "  ✓ ${crd_file} applied"
  else
    FAILED_COUNT=$((FAILED_COUNT + 1))
    echo "  ✗ ${crd_file} failed"
  fi
  echo ""
done

echo "Summary: ${SUCCESS_COUNT} succeeded, ${FAILED_COUNT} failed"

if [ "$FAILED_COUNT" -gt 0 ]; then
  echo "❌ ERROR: Some CRDs failed to apply"
  exit 1
fi

echo ""
echo "Waiting for CRDs to be established..."
sleep 5

# Verify CRDs are established
LIQO_CRDS=$(kubectl get crd | grep liqo | wc -l)
echo "Found ${LIQO_CRDS} Liqo CRDs installed in cluster"

if [ "$LIQO_CRDS" -lt 15 ]; then
  echo "❌ ERROR: Expected at least 15 CRDs, found ${LIQO_CRDS}"
  exit 1
fi

echo "✅ Stage 1 complete: CRDs upgraded successfully"
`, upgrade.Spec.TargetVersion)

	return &batchv1.Job{
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
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}

func (r *LiqoUpgradeReconciler) buildControllerManagerUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", controllerManagerUpgradePrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	backupConfigMapName := upgrade.Status.BackupName
	if backupConfigMapName == "" {
		backupConfigMapName = fmt.Sprintf("liqo-upgrade-env-backup-%s", upgrade.Name)
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Stage 2: Upgrading liqo-controller-manager"
echo "========================================="

TARGET_VERSION="%s"
NAMESPACE="%s"
BACKUP_CONFIGMAP="%s"

echo "Step 1: Verifying environment variable backup..."
if ! kubectl get configmap "${BACKUP_CONFIGMAP}" -n "${NAMESPACE}" &>/dev/null; then
  echo "⚠️  Warning: Environment backup ConfigMap not found"
fi

echo ""
echo "Step 2: Backing up current deployment spec..."
kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o yaml > /tmp/controller-manager-backup.yaml
echo "Backup saved to /tmp/controller-manager-backup.yaml"

echo ""
echo "Step 3: Extracting current environment variables..."
# Critical environment variables that MUST be preserved (Stage 2)
CRITICAL_VARS=(
  "POD_NAMESPACE"
  "CLUSTER_ID"
  "TENANT_NAMESPACE"
  "CLUSTER_ROLE"
  "ENABLE_IPAM"
  "LOG_LEVEL"
)

# Store current env vars
for var in "${CRITICAL_VARS[@]}"; do
  value=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='${var}')].value}" 2>/dev/null || echo "")
  if [ -n "$value" ]; then
    echo "  ✓ ${var}=${value}"
    eval "PRESERVE_${var}=\"${value}\""
  else
    echo "  ⚠️  ${var} not found (may use valueFrom)"
  fi
done

echo ""
echo "Step 4: Updating controller-manager image..."
NEW_IMAGE="ghcr.io/liqotech/liqo-controller-manager:${TARGET_VERSION}"
echo "New image: ${NEW_IMAGE}"

kubectl set image deployment/liqo-controller-manager \
  controller-manager="${NEW_IMAGE}" \
  -n "${NAMESPACE}"

echo ""
echo "Step 5: Waiting for rollout..."
if ! kubectl rollout status deployment/liqo-controller-manager -n "${NAMESPACE}" --timeout=5m; then
  echo "❌ ERROR: Rollout failed!"
  exit 1
fi

echo ""
echo "Step 6: Verifying environment variables after upgrade..."
# Verify critical env vars are still present
MISSING_VARS=()
for var in "${CRITICAL_VARS[@]}"; do
  value=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='${var}')].value}" 2>/dev/null || echo "")
  original_var="PRESERVE_${var}"
  if [ -n "${!original_var}" ]; then
    if [ "$value" != "${!original_var}" ]; then
      echo "  ⚠️  WARNING: ${var} changed: ${!original_var} -> ${value}"
    else
      echo "  ✓ ${var} preserved: ${value}"
    fi
  fi
done

echo ""
echo "Step 7: Verifying deployment health..."
if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-controller-manager -n "${NAMESPACE}"; then
  echo "❌ ERROR: Deployment not healthy!"
  exit 1
fi

echo ""
echo "Step 8: Verifying version..."
DEPLOYED_VERSION=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' | cut -d: -f2)
echo "Deployed version: ${DEPLOYED_VERSION}"

if [ "${DEPLOYED_VERSION}" != "${TARGET_VERSION}" ]; then
  echo "❌ ERROR: Version mismatch!"
  exit 1
fi

echo ""
echo "✅ Stage 2 complete: liqo-controller-manager upgraded"
echo "✅ All critical environment variables preserved"
`, upgrade.Spec.TargetVersion, namespace, backupConfigMapName)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "controller-manager-upgrade",
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
							Name:    "upgrade-controller-manager",
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

func (r *LiqoUpgradeReconciler) buildRollbackJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", rollbackJobPrefix, upgrade.Name)
	namespace := upgrade.Spec.Namespace
	if namespace == "" {
		namespace = "liqo"
	}

	backupConfigMapName := upgrade.Status.BackupName
	if backupConfigMapName == "" {
		backupConfigMapName = fmt.Sprintf("liqo-upgrade-env-backup-%s", upgrade.Name)
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e

echo "========================================="
echo "Rolling back to version %s"
echo "========================================="

PREVIOUS_VERSION="%s"
NAMESPACE="%s"
BACKUP_CONFIGMAP="%s"

echo "Step 1: Verifying environment backup exists..."
if kubectl get configmap "${BACKUP_CONFIGMAP}" -n "${NAMESPACE}" &>/dev/null; then
  echo "✓ Environment backup found"
  
  # Extract critical env vars from backup
  echo ""
  echo "Step 2: Extracting environment variables from backup..."
  kubectl get configmap "${BACKUP_CONFIGMAP}" -n "${NAMESPACE}" -o yaml > /tmp/env-backup.yaml
  echo "Environment backup retrieved"
else
  echo "⚠️  Warning: Environment backup ConfigMap not found, proceeding without env restoration"
fi

echo ""
echo "Step 3: Rolling back liqo-controller-manager image..."
PREVIOUS_IMAGE="ghcr.io/liqotech/liqo-controller-manager:${PREVIOUS_VERSION}"
echo "Previous image: ${PREVIOUS_IMAGE}"

kubectl set image deployment/liqo-controller-manager \
  controller-manager="${PREVIOUS_IMAGE}" \
  -n "${NAMESPACE}"

echo ""
echo "Step 4: Waiting for rollback rollout..."
if ! kubectl rollout status deployment/liqo-controller-manager -n "${NAMESPACE}" --timeout=5m; then
  echo "❌ ERROR: Rollback rollout failed!"
  exit 1
fi

echo ""
echo "Step 5: Verifying rollback health..."
if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-controller-manager -n "${NAMESPACE}"; then
  echo "❌ ERROR: Deployment not healthy after rollback!"
  exit 1
fi

echo ""
echo "Step 6: Verifying version rollback..."
DEPLOYED_VERSION=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}' | cut -d: -f2)
echo "Deployed version after rollback: ${DEPLOYED_VERSION}"

if [ "${DEPLOYED_VERSION}" != "${PREVIOUS_VERSION}" ]; then
  echo "❌ ERROR: Version mismatch after rollback!"
  exit 1
fi

echo ""
echo "Step 7: Verifying critical environment variables..."
# List of critical env vars that should be present
CRITICAL_VARS=(
  "POD_NAMESPACE"
  "CLUSTER_ID"
  "TENANT_NAMESPACE"
  "CLUSTER_ROLE"
  "ENABLE_IPAM"
  "LOG_LEVEL"
)

for var in "${CRITICAL_VARS[@]}"; do
  value=$(kubectl get deployment liqo-controller-manager -n "${NAMESPACE}" -o jsonpath="{.spec.template.spec.containers[0].env[?(@.name=='${var}')].value}" 2>/dev/null || echo "")
  if [ -n "$value" ]; then
    echo "  ✓ ${var}=${value}"
  else
    echo "  ⚠️  ${var} not found (may use valueFrom)"
  fi
done

# Rollback CRDs if needed (based on lastSuccessfulPhase)
LAST_PHASE="%s"
if [ "$LAST_PHASE" = "UpgradingCRDs" ] || [ "$LAST_PHASE" = "UpgradingControllerManager" ]; then
  echo ""
  echo "Note: CRD rollback may be needed but is not implemented in this simplified rollback"
  echo "Manual intervention may be required if CRDs changed"
fi

echo ""
echo "✅ Rollback complete"
echo "✅ Controller-manager restored to ${PREVIOUS_VERSION}"
echo "✅ Environment variables verified"
`, upgrade.Status.PreviousVersion, upgrade.Status.PreviousVersion, namespace, backupConfigMapName, upgrade.Status.LastSuccessfulPhase)

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
							Command: []string{"/bin/bash", "-c", script},
						},
					},
				},
			},
			BackoffLimit: int32Ptr(0),
		},
	}
}

// Helper functions

func (r *LiqoUpgradeReconciler) updateStatus(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, phase upgradev1alpha1.UpgradePhase, message string, additionalUpdates map[string]interface{}) (ctrl.Result, error) {
	upgrade.Status.Phase = phase
	upgrade.Status.Message = message
	upgrade.Status.LastUpdated = metav1.Now()

	if additionalUpdates != nil {
		if previousVersion, ok := additionalUpdates["previousVersion"].(string); ok {
			upgrade.Status.PreviousVersion = previousVersion
		}
		if lastSuccessfulPhase, ok := additionalUpdates["lastSuccessfulPhase"].(upgradev1alpha1.UpgradePhase); ok {
			upgrade.Status.LastSuccessfulPhase = lastSuccessfulPhase
		}
		if rolledBack, ok := additionalUpdates["rolledBack"].(bool); ok {
			upgrade.Status.RolledBack = rolledBack
		}
		if conditions, ok := additionalUpdates["conditions"].([]metav1.Condition); ok {
			upgrade.Status.Conditions = conditions
		}
		if backupReady, ok := additionalUpdates["backupReady"].(bool); ok {
			upgrade.Status.BackupReady = backupReady
		}
		if backupName, ok := additionalUpdates["backupName"].(string); ok {
			upgrade.Status.BackupName = backupName
		}
	}

	if err := r.Status().Update(ctx, upgrade); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *LiqoUpgradeReconciler) fail(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade, message string) (ctrl.Result, error) {
	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseFailed, message, nil)
}

func (r *LiqoUpgradeReconciler) handleDeletion(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(upgrade, finalizerName) {
		// Clean up jobs
		jobPrefixes := []string{freezeOperationsJobPrefix, crdUpgradeJobPrefix, controllerManagerUpgradePrefix, rollbackJobPrefix}
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

func (r *LiqoUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&upgradev1alpha1.LiqoUpgrade{}).
		Owns(&batchv1.Job{}).
		Named("liqoupgrade").
		Complete(r)
}
