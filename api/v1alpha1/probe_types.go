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

// ProbeSpec defines the desired state of Probe.
type ProbeSpec struct {
	// Type specifies the probe type (currently only "http" is supported)
	// +kubebuilder:validation:Enum=http
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// URL is the HTTP endpoint to check (required for HTTP probes)
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Format=uri
	// +optional
	URL string `json:"url,omitempty"`

	// Version is an optional version identifier for the probe
	// +optional
	Version string `json:"version,omitempty"`
}

// ProbeStatus defines the observed state of Probe.
type ProbeStatus struct {
	// State indicates the current state of the probe (e.g., "Success", "Failed", "Unknown")
	// +kubebuilder:validation:Enum=Success;Failed;Unknown
	// +kubebuilder:default=Unknown
	State string `json:"state,omitempty"`

	// LastCheckTime of the check
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// Message from the check (e.g., error details)
	Message string `json:"message,omitempty"`

	// SpecHash is a hash of the Probe spec to track changes
	SpecHash string `json:"specHash,omitempty"`

	// StatusCode is the status code returned by the probe (HTTP code, exit code, etc.)
	StatusCode int `json:"statusCode,omitempty"`

	// ResponseTime is the total response time (RTT) in seconds
	ResponseTime string `json:"responseTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pb
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Status",type=integer,JSONPath=`.status.statusCode`
// +kubebuilder:printcolumn:name="Last Check",type=date,JSONPath=`.status.lastCheckTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Probe is the Schema for the probes API.
type Probe struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProbeSpec   `json:"spec,omitempty"`
	Status ProbeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProbeList contains a list of Probe.
type ProbeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Probe `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Probe{}, &ProbeList{})
}
