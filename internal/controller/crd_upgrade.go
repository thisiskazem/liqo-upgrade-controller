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
