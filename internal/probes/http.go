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
	"fmt"
	"strconv"
	"strings"

	checkv1alpha1 "github.com/michaeljsaenz/healthcheck-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type HTTPProbeExecutor struct{}

func (e *HTTPProbeExecutor) Type() string {
	return "http"
}

func (h *HTTPProbeExecutor) BuildContainer(spec checkv1alpha1.ProbeSpec) (corev1.Container, error) {
	container := corev1.Container{
		Name:    "http-probe",
		Image:   "curlimages/curl:8.10.1",
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			fmt.Sprintf("curl -f --head --max-time 10 -w \"%%{http_code}|%%{time_total}\" -o /dev/null -s %s > /dev/termination-log", spec.URL),
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	return container, nil
}

func (h *HTTPProbeExecutor) ParseResult(message string) (Result, error) {
	result := Result{}

	if message == "" {
		return result, fmt.Errorf("empty termination message")
	}

	parts := strings.Split(strings.TrimSpace(message), "|")
	if len(parts) != 2 {
		return result, fmt.Errorf("invalid format: %q", message)
	}

	httpCode, err := strconv.Atoi(parts[0])
	if err != nil {
		return result, fmt.Errorf("invalid http code: %w", err)
	}

	responseTime, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return result, fmt.Errorf("invalid response time: %w", err)
	}

	result.StatusCode = httpCode
	result.ResponseTime = responseTime
	result.Success = httpCode >= 200 && httpCode < 400

	if !result.Success {
		result.ErrorMessage = fmt.Sprintf("HTTP %d", httpCode)
	}

	return result, nil
}
