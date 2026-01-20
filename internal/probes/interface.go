/*
Copyright 2025 Michael J. Saenz.

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

package probes

import (
	checkv1alpha1 "github.com/michaeljsaenz/healthcheck-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type Result struct {
	StatusCode   int
	ResponseTime float64
	Success      bool
	ErrorMessage string
}

type ProbeExecutor interface {
	Type() string
	BuildContainer(spec checkv1alpha1.ProbeSpec) (corev1.Container, error)
	ParseResult(message string) (Result, error)
}
