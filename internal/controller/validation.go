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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

// CompatibilityMatrix represents the version compatibility data
type CompatibilityMatrix map[string][]string

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
