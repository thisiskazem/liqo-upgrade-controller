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

// RemoteClusterVersion represents a remote cluster's Liqo version
type RemoteClusterVersion struct {
	// ClusterID is the unique identifier of the remote cluster
	// +kubebuilder:validation:Required
	ClusterID string `json:"clusterID"`

	// Version is the Liqo version running on the remote cluster
	// +kubebuilder:validation:Required
	Version string `json:"version"`
}

// LiqoUpgradeSpec defines the desired state of LiqoUpgrade
type LiqoUpgradeSpec struct {
	// CurrentVersion is the current Liqo version (e.g., "v1.0.0")
	// +kubebuilder:validation:Required
	CurrentVersion string `json:"currentVersion"`

	// TargetVersion is the desired Liqo version (e.g., "v1.0.1")
	// +kubebuilder:validation:Required
	TargetVersion string `json:"targetVersion"`

	// Namespace where Liqo is installed (default: "liqo")
	// +kubebuilder:default="liqo"
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// RemoteClusterVersions lists the Liqo versions of remote peered clusters (optional)
	// If not provided, the controller assumes remote clusters are running the same version as local cluster
	// +optional
	RemoteClusterVersions []RemoteClusterVersion `json:"remoteClusterVersions,omitempty"`
}

// UpgradePhase represents the current phase of the upgrade process
type UpgradePhase string

const (
	// PhaseNone means upgrade hasn't started
	PhaseNone UpgradePhase = ""

	// PhaseCompatibilityCheck means we're checking version compatibility
	PhaseCompatibilityCheck UpgradePhase = "CheckingCompatibility"

	// PhaseBackup means we're creating backup
	PhaseBackup UpgradePhase = "CreatingBackup"

	// PhaseCRDs means we're upgrading and verifying CRDs
	PhaseCRDs UpgradePhase = "UpgradingCRDs"

	// PhaseControlPlane means we're upgrading and verifying control plane components
	PhaseControlPlane UpgradePhase = "UpgradingControlPlane"

	// PhaseRollingBack means we're rolling back
	PhaseRollingBack UpgradePhase = "RollingBack"

	// PhaseCompleted means upgrade succeeded
	PhaseCompleted UpgradePhase = "Completed"

	// PhaseFailed means upgrade failed
	PhaseFailed UpgradePhase = "Failed"
)

// LiqoUpgradeStatus defines the observed state of LiqoUpgrade
type LiqoUpgradeStatus struct {
	// Phase indicates the current phase of the upgrade
	// +optional
	Phase UpgradePhase `json:"phase,omitempty"`

	// Message provides human-readable details about the current phase
	// +optional
	Message string `json:"message,omitempty"`

	// LastUpdated is the timestamp of the last status update
	// +optional
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// BackupName references the backup job created before upgrade
	// +optional
	BackupName string `json:"backupName,omitempty"`

	// BackupReady indicates if backup is available for rollback
	// +optional
	BackupReady bool `json:"backupReady,omitempty"`

	// LastSuccessfulPhase is the last phase that completed successfully
	// +optional
	LastSuccessfulPhase UpgradePhase `json:"lastSuccessfulPhase,omitempty"`

	// RolledBack indicates if this upgrade was rolled back
	// +optional
	RolledBack bool `json:"rolledBack,omitempty"`

	// DetectedLocalVersion is the actual version detected on the local cluster
	// +optional
	DetectedLocalVersion string `json:"detectedLocalVersion,omitempty"`

	// LowestVersion is the lowest version among local and remote clusters
	// +optional
	LowestVersion string `json:"lowestVersion,omitempty"`

	// CompatibilityCheckPassed indicates if compatibility check succeeded
	// +optional
	CompatibilityCheckPassed *bool `json:"compatibilityCheckPassed,omitempty"`

	// conditions represent the current state of the LiqoUpgrade resource
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.spec.currentVersion`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LiqoUpgrade is the Schema for the liqoupgrades API
type LiqoUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LiqoUpgradeSpec   `json:"spec"`
	Status LiqoUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LiqoUpgradeList contains a list of LiqoUpgrade
type LiqoUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiqoUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiqoUpgrade{}, &LiqoUpgradeList{})
}
