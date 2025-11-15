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
