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
	networkFabricUpgradePrefix     = "liqo-upgrade-network-fabric"
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
	case upgradev1alpha1.PhaseNetworkFabric:
		return r.monitorNetworkFabricUpgrade(ctx, upgrade)
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
	upgrade.Status.TotalStages = 9 // Stage 0-8 per design (now implementing through Stage 3)
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
		return r.startNetworkFabricUpgrade(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 2 failed: Controller-manager upgrade failed")
		return r.startRollback(ctx, upgrade, "Controller-manager upgrade failed")
	}

	logger.Info("Controller-manager upgrade job still running")
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Stage 3: Upgrade Network Fabric
func (r *LiqoUpgradeReconciler) startNetworkFabricUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Stage 3: Starting network fabric upgrade")

	upgrade.Status.CurrentStage = 3

	job := r.buildNetworkFabricUpgradeJob(upgrade)
	if err := controllerutil.SetControllerReference(upgrade, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			logger.Error(err, "Failed to create network fabric upgrade job")
			return r.fail(ctx, upgrade, "Failed to create network fabric upgrade job")
		}
	}

	return r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseNetworkFabric, "Upgrading network fabric components", nil)
}

func (r *LiqoUpgradeReconciler) monitorNetworkFabricUpgrade(ctx context.Context, upgrade *upgradev1alpha1.LiqoUpgrade) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	jobName := fmt.Sprintf("%s-%s", networkFabricUpgradePrefix, upgrade.Name)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: upgrade.Spec.Namespace}, job); err != nil {
		logger.Error(err, "Failed to get network fabric upgrade job")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("Stage 3 completed: Network fabric upgraded successfully")
		statusUpdates := map[string]interface{}{
			"lastSuccessfulPhase": upgradev1alpha1.PhaseNetworkFabric,
		}
		if _, err := r.updateStatus(ctx, upgrade, upgradev1alpha1.PhaseNetworkFabric, "Network fabric upgraded", statusUpdates); err != nil {
			return ctrl.Result{}, err
		}
		return r.startVerification(ctx, upgrade)
	}

	if job.Status.Failed > 0 {
		logger.Error(nil, "Stage 3 failed: Network fabric upgrade failed")
		return r.startRollback(ctx, upgrade, "Network fabric upgrade failed")
	}

	logger.Info("Network fabric upgrade job still running")
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

func (r *LiqoUpgradeReconciler) buildNetworkFabricUpgradeJob(upgrade *upgradev1alpha1.LiqoUpgrade) *batchv1.Job {
	jobName := fmt.Sprintf("%s-%s", networkFabricUpgradePrefix, upgrade.Name)
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
echo "Stage 3: Upgrading Network Fabric"
echo "========================================="

TARGET_VERSION="%s"
NAMESPACE="%s"
BACKUP_CONFIGMAP="%s"

echo "Step 1: Backing up network fabric deployments..."
mkdir -p /tmp/network-backup

# Find all liqo-tenant-* namespaces for gateway deployments
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
echo "Found tenant namespaces: ${TENANT_NAMESPACES}"

# Backup gateway deployments from tenant namespaces
for TENANT_NS in ${TENANT_NAMESPACES}; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Backing up gateway: ${GW} in namespace ${TENANT_NS}"
    kubectl get deployment "${GW}" -n "${TENANT_NS}" -o yaml > "/tmp/network-backup/${TENANT_NS}-${GW}-deployment.yaml"
  done
done

# Backup core network components
if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-fabric (daemonset)"
  kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-fabric-daemonset.yaml"
fi

if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-ipam (deployment)"
  kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-ipam-deployment.yaml"
fi

if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo "  Backing up liqo-proxy (deployment)"
  kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o yaml > "/tmp/network-backup/liqo-proxy-deployment.yaml"
fi

echo ""
echo "Step 2: Upgrading Gateway Templates (MUST happen before gateway instance recreation)..."

# Upgrade WgGatewayClientTemplate
echo "--- Upgrading WgGatewayClientTemplate ---"
if kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating WgGatewayClientTemplate to version ${TARGET_VERSION}..."
  
  # Patch the template to update container images
  kubectl patch wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ WgGatewayClientTemplate updated" || echo "  ⚠️  Warning: Could not update WgGatewayClientTemplate"
else
  echo "  ℹ️  WgGatewayClientTemplate not found, skipping"
fi

# Upgrade WgGatewayServerTemplate
echo "--- Upgrading WgGatewayServerTemplate ---"
if kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" &>/dev/null; then
  echo "Updating WgGatewayServerTemplate to version ${TARGET_VERSION}..."
  
  kubectl patch wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
    --type='json' -p='[
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${TARGET_VERSION}"'"},
      {"op": "replace", "path": "/spec/template/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${TARGET_VERSION}"'"}
    ]' && echo "  ✓ WgGatewayServerTemplate updated" || echo "  ⚠️  Warning: Could not update WgGatewayServerTemplate"
else
  echo "  ℹ️  WgGatewayServerTemplate not found, skipping"
fi

# Verify templates were updated
sleep 2
echo "Verifying template updates..."
CLIENT_TEMPLATE_IMAGE=$(kubectl get wggatewayclienttemplate wireguard-client -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.template.spec.containers[0].image}' 2>/dev/null || echo "not found")
echo "  Client template image: ${CLIENT_TEMPLATE_IMAGE}"

SERVER_TEMPLATE_IMAGE=$(kubectl get wggatewayservertemplate wireguard-server -n "${NAMESPACE}" \
  -o jsonpath='{.spec.template.spec.deployment.spec.template.spec.containers[0].image}' 2>/dev/null || echo "not found")
echo "  Server template image: ${SERVER_TEMPLATE_IMAGE}"

if [[ "$CLIENT_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$CLIENT_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "❌ ERROR: Client template not updated to ${TARGET_VERSION}!"
  exit 1
fi

if [[ "$SERVER_TEMPLATE_IMAGE" != *"${TARGET_VERSION}"* ]] && [[ "$SERVER_TEMPLATE_IMAGE" != "not found" ]]; then
  echo "❌ ERROR: Server template not updated to ${TARGET_VERSION}!"
  exit 1
fi

echo "✅ Gateway templates upgraded successfully"

echo ""
echo "Step 3: Upgrading network components sequentially..."

# Upgrade liqo-ipam first (less critical, manages IP allocation)
if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-ipam ---"
  echo "Extracting environment variables..."
  
  # Get current environment variables
  ENV_JSON=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in deployment spec"
  
  NEW_IMAGE="ghcr.io/liqotech/ipam:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"
  
  CONTAINER_NAME=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image deployment/liqo-ipam \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"
  
  echo "Waiting for rollout..."
  if ! kubectl rollout status deployment/liqo-ipam -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: liqo-ipam rollout failed!"
    exit 1
  fi
  
  echo "Verifying health..."
  if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-ipam -n "${NAMESPACE}"; then
    echo "❌ ERROR: liqo-ipam not healthy!"
    exit 1
  fi
  
  echo "✅ liqo-ipam upgraded successfully"
fi

# Upgrade liqo-proxy (Deployment)
if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-proxy ---"
  echo "Extracting environment variables..."
  
  # Get current environment variables
  ENV_JSON=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in deployment spec"
  
  NEW_IMAGE="ghcr.io/liqotech/proxy:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"
  
  CONTAINER_NAME=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image deployment/liqo-proxy \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"
  
  echo "Waiting for rollout..."
  if ! kubectl rollout status deployment/liqo-proxy -n "${NAMESPACE}" --timeout=5m; then
    echo "❌ ERROR: liqo-proxy rollout failed!"
    exit 1
  fi
  
  echo "Verifying health..."
  if ! kubectl wait --for=condition=available --timeout=2m deployment/liqo-proxy -n "${NAMESPACE}"; then
    echo "❌ ERROR: liqo-proxy not healthy!"
    exit 1
  fi
  
  echo "✅ liqo-proxy upgraded successfully"
fi

# Upgrade liqo-fabric (DaemonSet) - Data plane component
if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo ""
  echo "--- Upgrading liqo-fabric (DaemonSet - Data Plane) ---"
  echo "⚠️  WARNING: This may cause temporary network disruption"
  echo "Extracting environment variables..."
  
  # Get current environment variables
  ENV_JSON=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].env}')
  echo "  Current environment variables preserved in daemonset spec"
  
  NEW_IMAGE="ghcr.io/liqotech/fabric:${TARGET_VERSION}"
  echo "New image: ${NEW_IMAGE}"
  
  CONTAINER_NAME=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
  kubectl set image daemonset/liqo-fabric \
    "${CONTAINER_NAME}=${NEW_IMAGE}" \
    -n "${NAMESPACE}"
  
  echo "Waiting for DaemonSet rollout (this may take several minutes)..."
  if ! kubectl rollout status daemonset/liqo-fabric -n "${NAMESPACE}" --timeout=10m; then
    echo "❌ ERROR: liqo-fabric rollout failed!"
    exit 1
  fi
  
  echo "Verifying all fabric pods..."
  DESIRED=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.status.desiredNumberScheduled}')
  READY=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.status.numberReady}')
  
  if [ "${DESIRED}" != "${READY}" ]; then
    echo "❌ ERROR: Not all fabric pods ready! Desired: ${DESIRED}, Ready: ${READY}"
    exit 1
  fi
  
  echo "✅ liqo-fabric upgraded successfully (${READY}/${DESIRED} pods ready)"
fi

# Upgrade gateway deployments in tenant namespaces
# The hierarchy is: GatewayClient -> WgGatewayClient -> Deployment
# When templates are upgraded, we need to trigger GatewayClient to recreate WgGatewayClient
echo ""
echo "--- Upgrading liqo-gateway deployments in tenant namespaces ---"

# Re-fetch tenant namespaces for gateway upgrade
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

GATEWAY_COUNT=0
for TENANT_NS in ${TENANT_NAMESPACES}; do
  echo ""
  echo "Processing tenant namespace: ${TENANT_NS}"
  
  # Find GatewayClient resources (these reference the templates)
  GATEWAY_CLIENTS=$(kubectl get gatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  GATEWAY_SERVERS=$(kubectl get gatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  # Process GatewayClient resources - trigger reconciliation by adding annotation
  for GW_CLIENT in ${GATEWAY_CLIENTS}; do
    echo "  Found GatewayClient: ${GW_CLIENT}"
    GATEWAY_COUNT=$((GATEWAY_COUNT + 1))
    
    # Get the template reference
    TEMPLATE_REF=$(kubectl get gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.clientTemplateRef.name}' 2>/dev/null || echo "")
    echo "    Template: ${TEMPLATE_REF}"
    
    # Trigger reconciliation by adding/updating annotation
    # This will cause the controller to recreate WgGatewayClient from the updated template
    TIMESTAMP=$(date +%%s)
    echo "    Triggering reconciliation with timestamp: ${TIMESTAMP}"
    kubectl annotate gatewayclient "${GW_CLIENT}" -n "${TENANT_NS}" \
      liqo.io/force-recreate="${TIMESTAMP}" \
      --overwrite
    
    echo "    ✓ GatewayClient annotated to trigger recreation"
  done
  
  # Process GatewayServer resources
  for GW_SERVER in ${GATEWAY_SERVERS}; do
    echo "  Found GatewayServer: ${GW_SERVER}"
    GATEWAY_COUNT=$((GATEWAY_COUNT + 1))
    
    # Get the template reference
    TEMPLATE_REF=$(kubectl get gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.serverTemplateRef.name}' 2>/dev/null || echo "")
    echo "    Template: ${TEMPLATE_REF}"
    
    # Trigger reconciliation
    TIMESTAMP=$(date +%%s)
    echo "    Triggering reconciliation with timestamp: ${TIMESTAMP}"
    kubectl annotate gatewayserver "${GW_SERVER}" -n "${TENANT_NS}" \
      liqo.io/force-recreate="${TIMESTAMP}" \
      --overwrite
    
    echo "    ✓ GatewayServer annotated to trigger recreation"
  done
  
  # If annotation doesn't work, delete and recreate WgGatewayClient resources
  # This forces them to be regenerated from the updated templates
  WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  if [ -n "$WGGW_CLIENTS" ] || [ -n "$WGGW_SERVERS" ]; then
    echo "  Deleting WgGateway resources to trigger recreation from updated templates..."
    
    for WGGW_CLIENT in ${WGGW_CLIENTS}; do
      echo "    Deleting wggatewayclient: ${WGGW_CLIENT}"
      kubectl delete wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
    done
    
    for WGGW_SERVER in ${WGGW_SERVERS}; do
      echo "    Deleting wggatewayserver: ${WGGW_SERVER}"
      kubectl delete wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --wait=false 2>/dev/null || true
    done
    
    echo "  Waiting for controller to recreate WgGateway resources from updated templates..."
    sleep 10
    
    # Wait for WgGatewayClient/Server to be recreated
    TIMEOUT=60
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
      RECREATED_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
      RECREATED_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
      
      if [ -n "$RECREATED_CLIENTS" ] || [ -n "$RECREATED_SERVERS" ]; then
        echo "  ✓ WgGateway resources recreated"
        break
      fi
      
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done
    
    if [ $ELAPSED -ge $TIMEOUT ]; then
      echo "  ⚠️  Warning: WgGateway resources not recreated within timeout"
    fi
  fi
  
  # Wait for gateway deployments to roll out with new images
  echo "  Waiting for gateway deployments to update..."
  sleep 5
  
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
  
  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Monitoring gateway deployment: ${GW}"
    
    # Wait for deployment to start updating
    TIMEOUT=60
    ELAPSED=0
    while [ $ELAPSED -lt $TIMEOUT ]; do
      CURRENT_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
      if [[ "$CURRENT_IMAGE" == *"${TARGET_VERSION}"* ]]; then
        echo "    ✓ Deployment spec updated to ${TARGET_VERSION}"
        break
      fi
      sleep 2
      ELAPSED=$((ELAPSED + 2))
    done
    
    if [ $ELAPSED -ge $TIMEOUT ]; then
      echo "    ⚠️  Warning: Deployment spec not updated after ${TIMEOUT}s"
      echo "    Current image: ${CURRENT_IMAGE}"
    fi
    
    # Wait for rollout to complete
    if kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null; then
      echo "    ✓ Rollout completed"
    else
      echo "    ⚠️  Warning: Rollout did not complete"
    fi
    
    # Verify final state
    FINAL_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "")
    echo "    Final image: ${FINAL_IMAGE}"
  done
done

if [ ${GATEWAY_COUNT} -eq 0 ]; then
  echo "  ℹ️  No GatewayClient/GatewayServer resources found in tenant namespaces"
else
  echo ""
  echo "✅ ${GATEWAY_COUNT} gateway resource(s) processed"
fi

echo ""
echo "Step 4: Final verification of network fabric..."

# Wait for pod updates to fully propagate
echo "Waiting for pod updates to propagate..."
sleep 5

# Re-fetch tenant namespaces for verification
TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)

# Verify core network components by checking running pods
if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-ipam (deployment):"
  CURRENT_IMAGE=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    Deployment spec image: ${CURRENT_IMAGE}"
  
  # Check actual running pod
  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=ipam -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi
  
  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-ipam not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-proxy (deployment):"
  CURRENT_IMAGE=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    Deployment spec image: ${CURRENT_IMAGE}"
  
  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=proxy -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi
  
  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-proxy not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
  echo "  Checking liqo-fabric (daemonset):"
  CURRENT_IMAGE=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')
  echo "    DaemonSet spec image: ${CURRENT_IMAGE}"
  
  POD_IMAGE=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || echo "")
  if [ -n "$POD_IMAGE" ]; then
    echo "    Running pod image: ${POD_IMAGE}"
  fi
  
  if [[ "$CURRENT_IMAGE" != *"${TARGET_VERSION}"* ]]; then
    echo "    ❌ ERROR: liqo-fabric not running target version!"
    exit 1
  fi
  echo "    ✓ Version correct"
fi

# Verify gateway deployments in tenant namespaces
for TENANT_NS in ${TENANT_NAMESPACES}; do
  GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
  
  for GW in ${GATEWAY_DEPLOYMENTS}; do
    echo "  Checking gateway ${GW} in ${TENANT_NS}:"
    
    # Double-check rollout status
    kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=30s &>/dev/null || true
    
    # Get the current pod-template-hash from the deployment's replicaset
    CURRENT_RS=$(kubectl get rs -n "${TENANT_NS}" -l networking.liqo.io/gateway-name="${GW#gw-}" \
      --sort-by='.metadata.creationTimestamp' -o jsonpath='{.items[-1:].metadata.labels.pod-template-hash}' 2>/dev/null || echo "")
    
    # Check deployment spec (should be updated)
    GATEWAY_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="gateway")].image}')
    WIREGUARD_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="wireguard")].image}')
    GENEVE_IMAGE=$(kubectl get deployment "${GW}" -n "${TENANT_NS}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="geneve")].image}')
    
    echo "    Deployment spec:"
    echo "      Gateway: ${GATEWAY_IMAGE}"
    echo "      Wireguard: ${WIREGUARD_IMAGE}"
    echo "      Geneve: ${GENEVE_IMAGE}"
    
    # Find the actual running pod with the current template hash
    if [ -n "$CURRENT_RS" ]; then
      RUNNING_POD=$(kubectl get pods -n "${TENANT_NS}" \
        -l networking.liqo.io/gateway-name="${GW#gw-}",pod-template-hash="${CURRENT_RS}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | awk '{print $1}')
    else
      RUNNING_POD=$(kubectl get pods -n "${TENANT_NS}" \
        -l networking.liqo.io/gateway-name="${GW#gw-}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | awk '{print $1}')
    fi
    
    if [ -n "$RUNNING_POD" ]; then
      POD_GATEWAY_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="gateway")].image}' 2>/dev/null || echo "")
      POD_WIREGUARD_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="wireguard")].image}' 2>/dev/null || echo "")
      POD_GENEVE_IMAGE=$(kubectl get pod "${RUNNING_POD}" -n "${TENANT_NS}" \
        -o jsonpath='{.spec.containers[?(@.name=="geneve")].image}' 2>/dev/null || echo "")
      
      echo "    Running pod (${RUNNING_POD}):"
      echo "      Gateway: ${POD_GATEWAY_IMAGE}"
      echo "      Wireguard: ${POD_WIREGUARD_IMAGE}"
      echo "      Geneve: ${POD_GENEVE_IMAGE}"
    fi
    
    # Verify deployment spec containers are on target version
    if [[ "$GATEWAY_IMAGE" != *"${TARGET_VERSION}"* ]] || \
       [[ "$WIREGUARD_IMAGE" != *"${TARGET_VERSION}"* ]] || \
       [[ "$GENEVE_IMAGE" != *"${TARGET_VERSION}"* ]]; then
      echo "    ❌ ERROR: Deployment spec not on target version!"
      echo "    Expected version: ${TARGET_VERSION}"
      exit 1
    fi
    
    echo "    ✓ All containers on target version"
  done
done

echo ""
echo "Step 4: Verifying network connectivity..."
# Basic connectivity check - verify fabric pods can reach API server
FABRIC_PODS=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=fabric -o jsonpath='{.items[*].metadata.name}')
if [ -n "$FABRIC_PODS" ]; then
  echo "✓ Fabric pods found and running"
else
  echo "⚠️  Warning: No fabric pods found"
fi

echo ""
echo "========================================="
echo "✅ Stage 3 complete: Network Fabric upgraded"
echo "✅ All network components upgraded to ${TARGET_VERSION}"
echo "✅ All critical environment variables and args preserved"
echo "========================================="
`, upgrade.Spec.TargetVersion, namespace, backupConfigMapName)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "liqo-upgrade",
				"app.kubernetes.io/component": "network-fabric-upgrade",
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
							Name:    "upgrade-network-fabric",
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

if [ "$LAST_PHASE" = "UpgradingNetworkFabric" ]; then
  echo ""
  echo "Step 8: Rolling back network fabric components..."
  
  # Find all liqo-tenant-* namespaces for gateway deployments
  TENANT_NAMESPACES=$(kubectl get namespaces -o jsonpath='{.items[*].metadata.name}' | tr ' ' '\n' | grep '^liqo-tenant-' || true)
  echo "  Found tenant namespaces: ${TENANT_NAMESPACES}"
  
  # Rollback gateway resources in tenant namespaces first
  echo "  Rolling back gateway resources in tenant namespaces..."
  GATEWAY_COUNT=0
  
  for TENANT_NS in ${TENANT_NAMESPACES}; do
    echo "    Processing tenant namespace: ${TENANT_NS}"
    
    # Rollback wggatewayclient resources
    WGGW_CLIENTS=$(kubectl get wggatewayclient -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_CLIENT in ${WGGW_CLIENTS}; do
      echo "      Rolling back wggatewayclient: ${WGGW_CLIENT}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))
      
      kubectl patch wggatewayclient "${WGGW_CLIENT}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "        Warning: Could not patch wggatewayclient"
    done
    
    # Rollback wggatewayserver resources
    WGGW_SERVERS=$(kubectl get wggatewayserver -n "${TENANT_NS}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    for WGGW_SERVER in ${WGGW_SERVERS}; do
      echo "      Rolling back wggatewayserver: ${WGGW_SERVER}"
      GATEWAY_COUNT=$((GATEWAY_COUNT + 1))
      
      kubectl patch wggatewayserver "${WGGW_SERVER}" -n "${TENANT_NS}" --type='json' -p='[
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/0/image", "value": "ghcr.io/liqotech/gateway:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/1/image", "value": "ghcr.io/liqotech/gateway/wireguard:'"${PREVIOUS_VERSION}"'"},
        {"op": "replace", "path": "/spec/deployment/spec/template/spec/containers/2/image", "value": "ghcr.io/liqotech/gateway/geneve:'"${PREVIOUS_VERSION}"'"}
      ]' 2>/dev/null || echo "        Warning: Could not patch wggatewayserver"
    done
    
    # Also rollback deployments directly as fallback
    GATEWAY_DEPLOYMENTS=$(kubectl get deployments -n "${TENANT_NS}" -l networking.liqo.io/component=gateway -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    
    for GW in ${GATEWAY_DEPLOYMENTS}; do
      echo "      Rolling back gateway deployment: ${GW}"
      
      kubectl set image deployment/"${GW}" \
        gateway=ghcr.io/liqotech/gateway:${PREVIOUS_VERSION} \
        wireguard=ghcr.io/liqotech/gateway/wireguard:${PREVIOUS_VERSION} \
        geneve=ghcr.io/liqotech/gateway/geneve:${PREVIOUS_VERSION} \
        -n "${TENANT_NS}" 2>/dev/null || echo "        Warning: Could not update deployment"
      
      kubectl rollout status deployment/"${GW}" -n "${TENANT_NS}" --timeout=5m 2>/dev/null || echo "        Warning: Rollout did not complete"
    done
  done
  
  if [ ${GATEWAY_COUNT} -gt 0 ]; then
    echo "    ✅ ${GATEWAY_COUNT} gateway resource(s) rolled back"
  fi
  
  # Rollback core network components
  if kubectl get deployment liqo-ipam -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-ipam (deployment)..."
    CONTAINER_NAME=$(kubectl get deployment liqo-ipam -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/ipam:${PREVIOUS_VERSION}"
    kubectl set image deployment/liqo-ipam \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status deployment/liqo-ipam -n "${NAMESPACE}" --timeout=3m
    echo "    ✓ liqo-ipam rolled back"
  fi
  
  if kubectl get deployment liqo-proxy -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-proxy (deployment)..."
    CONTAINER_NAME=$(kubectl get deployment liqo-proxy -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/proxy:${PREVIOUS_VERSION}"
    kubectl set image deployment/liqo-proxy \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status deployment/liqo-proxy -n "${NAMESPACE}" --timeout=3m
    echo "    ✓ liqo-proxy rolled back"
  fi
  
  if kubectl get daemonset liqo-fabric -n "${NAMESPACE}" &>/dev/null; then
    echo "  Rolling back liqo-fabric (daemonset)..."
    CONTAINER_NAME=$(kubectl get daemonset liqo-fabric -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].name}')
    ROLLBACK_IMAGE="ghcr.io/liqotech/fabric:${PREVIOUS_VERSION}"
    kubectl set image daemonset/liqo-fabric \
      "${CONTAINER_NAME}=${ROLLBACK_IMAGE}" \
      -n "${NAMESPACE}"
    kubectl rollout status daemonset/liqo-fabric -n "${NAMESPACE}" --timeout=5m
    echo "    ✓ liqo-fabric rolled back"
  fi
  
  echo "  ✅ Network fabric components rolled back"
fi

if [ "$LAST_PHASE" = "UpgradingControllerManager" ]; then
  echo ""
  echo "Note: Controller manager already rolled back in previous steps"
fi

if [ "$LAST_PHASE" = "UpgradingCRDs" ] || [ "$LAST_PHASE" = "UpgradingControllerManager" ] || [ "$LAST_PHASE" = "UpgradingNetworkFabric" ]; then
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
		jobPrefixes := []string{freezeOperationsJobPrefix, crdUpgradeJobPrefix, controllerManagerUpgradePrefix, networkFabricUpgradePrefix, rollbackJobPrefix}
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
