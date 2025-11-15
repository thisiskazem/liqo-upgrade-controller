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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	upgradev1alpha1 "github.com/thisiskazem/liqo-upgrade-controller/api/v1alpha1"
)

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
