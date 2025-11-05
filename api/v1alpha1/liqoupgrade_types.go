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

type RemoteClusterVersion struct {
	ClusterID string `json:"clusterID"`
	Version   string `json:"version"`
}

type LiqoUpgradeSpec struct {
	CurrentVersion        string                 `json:"currentVersion"`
	TargetVersion         string                 `json:"targetVersion"`
	Namespace             string                 `json:"namespace,omitempty"`
	RemoteClusterVersions []RemoteClusterVersion `json:"remoteClusterVersions,omitempty"`
}

type UpgradePhase string

const (
	PhaseNone                 UpgradePhase = ""
	PhaseCompatibilityCheck   UpgradePhase = "CheckingCompatibility"
	PhaseBackup               UpgradePhase = "CreatingBackup"
	PhaseCRDs                 UpgradePhase = "UpgradingCRDs"
	PhaseControlPlane         UpgradePhase = "UpgradingControlPlane"
	PhaseExtendedControlPlane UpgradePhase = "UpgradingExtendedControlPlane"
	PhaseDataPlane            UpgradePhase = "UpgradingDataPlane"
	PhaseRollingBack          UpgradePhase = "RollingBack"
	PhaseCompleted            UpgradePhase = "Completed"
	PhaseFailed               UpgradePhase = "Failed"
)

type LiqoUpgradeStatus struct {
	Phase                    UpgradePhase       `json:"phase,omitempty"`
	Message                  string             `json:"message,omitempty"`
	LastUpdated              metav1.Time        `json:"lastUpdated,omitempty"`
	BackupName               string             `json:"backupName,omitempty"`
	BackupReady              bool               `json:"backupReady,omitempty"`
	LastSuccessfulPhase      UpgradePhase       `json:"lastSuccessfulPhase,omitempty"`
	RolledBack               bool               `json:"rolledBack,omitempty"`
	DetectedLocalVersion     string             `json:"detectedLocalVersion,omitempty"`
	LowestVersion            string             `json:"lowestVersion,omitempty"`
	CompatibilityCheckPassed *bool              `json:"compatibilityCheckPassed,omitempty"`
	Conditions               []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.spec.currentVersion`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type LiqoUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LiqoUpgradeSpec   `json:"spec"`
	Status            LiqoUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type LiqoUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LiqoUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LiqoUpgrade{}, &LiqoUpgradeList{})
}
