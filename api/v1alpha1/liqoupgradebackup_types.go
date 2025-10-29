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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LiqoUpgradeBackupSpec defines the backup data
type LiqoUpgradeBackupSpec struct {
	// UpgradeName references the LiqoUpgrade this backup belongs to
	// +kubebuilder:validation:Required
	UpgradeName string `json:"upgradeName"`

	// Phase indicates which phase this backup is for
	// +kubebuilder:validation:Required
	Phase UpgradePhase `json:"phase"`

	// Version is the version backed up
	// +kubebuilder:validation:Required
	Version string `json:"version"`

	// BackupTime when the backup was created
	// +kubebuilder:validation:Required
	BackupTime metav1.Time `json:"backupTime"`

	// CRDManifests stores the YAML manifests of CRDs
	// +optional
	CRDManifests map[string]string `json:"crdManifests,omitempty"`

	// DeploymentManifests stores deployment manifests (for future phases)
	// +optional
	DeploymentManifests map[string]string `json:"deploymentManifests,omitempty"`
}

// LiqoUpgradeBackupStatus defines the observed state
type LiqoUpgradeBackupStatus struct {
	// Ready indicates if backup is ready for rollback
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message provides details
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=liqobackup
// +kubebuilder:printcolumn:name="Upgrade",type=string,JSONPath=`.spec.upgradeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LiqoUpgradeBackup stores backup state for rollback
type LiqoUpgradeBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LiqoUpgradeBackupSpec   `json:"spec"`
	Status LiqoUpgradeBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiqoUpgradeBackupList contains a list of LiqoUpgradeBackup
type LiqoUpgradeBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiqoUpgradeBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiqoUpgradeBackup{}, &LiqoUpgradeBackupList{})
}
