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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	checkv1alpha1 "github.com/michaeljsaenz/healthcheck-operator/api/v1alpha1"
	"github.com/michaeljsaenz/healthcheck-operator/internal/probes"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// Finalizer for cleanup
	probeFinalizer = "probe.check.x-health.io/finalizer"

	// Annotation key for spec hash
	specHashAnnotation = "probe.check.x-health.io/spec-hash"

	// Timeouts and intervals
	requeueOnConflict    = 2 * time.Second
	requeueAfterCreation = 10 * time.Second
	jobCreationTimeout   = 5 * time.Minute
	jobActiveDeadline    = int64(60)  // 60 seconds
	jobTTLAfterFinished  = int32(300) // 5 minutes
)

// Metrics
var (
	probeState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_state",
			Help: "State of the probe (1 = Success, 0 = Failed)",
		},
		[]string{"namespace", "name", "type", "version", "status_code", "url"},
	)

	probeStatusCode = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_status_code",
			Help: "Status code returned by the probe (HTTP code, exit code, etc.)",
		},
		[]string{"namespace", "name", "type", "version", "status_code", "url"},
	)

	probeResponseTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_response_time_seconds",
			Help: "Total response time (RTT) for the probe in seconds",
		},
		[]string{"namespace", "name", "type", "version", "status_code", "url"},
	)

	probePodAttempts = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_pod_attempts_total",
			Help: "Number of pod attempts for the probe (Job backoff retries)",
		},
		[]string{"namespace", "name", "type", "version", "status_code", "url"},
	)
)

// Register metrics and makes available at /metrics endpoint
func init() {
	metrics.Registry.MustRegister(probeState)
	metrics.Registry.MustRegister(probeStatusCode)
	metrics.Registry.MustRegister(probeResponseTime)
	metrics.Registry.MustRegister(probePodAttempts)
}

// ProbeReconciler reconciles a Probe object
type ProbeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=check.x-health.io,resources=probes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=check.x-health.io,resources=probes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=check.x-health.io,resources=probes/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile reconciles a Probe object
func (r *ProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Declare a Probe instance
	var probe checkv1alpha1.Probe
	// Get the Probe instance
	if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
		if errors.IsNotFound(err) {
			// Probe was deleted, metrics cleanup handled by finalizer
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, err
	}

	// Handle finalizer for metric cleanup
	if probe.ObjectMeta.DeletionTimestamp.IsZero() {
		// Object is not being deleted, ensure finalizer is present
		if !controllerutil.ContainsFinalizer(&probe, probeFinalizer) {
			controllerutil.AddFinalizer(&probe, probeFinalizer)
			if err := r.Update(ctx, &probe); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// Object is being deleted
		if controllerutil.ContainsFinalizer(&probe, probeFinalizer) {
			// Clean up metrics using partial match to remove all series for this probe
			labels := prometheus.Labels{"namespace": probe.Namespace, "name": probe.Name}
			probeState.DeletePartialMatch(labels)
			probeStatusCode.DeletePartialMatch(labels)
			probeResponseTime.DeletePartialMatch(labels)
			probePodAttempts.DeletePartialMatch(labels)
			logger.Info("Cleaned up metrics for deleted probe", "probe", probe.Name)

			// Remove finalizer
			controllerutil.RemoveFinalizer(&probe, probeFinalizer)
			if err := r.Update(ctx, &probe); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if the Job already exists
	jobName := probe.Name + "-job"
	var job batchv1.Job
	jobExists := true

	// Try to get the Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: probe.Namespace}, &job)
	if err != nil && errors.IsNotFound(err) {
		jobExists = false
	} else if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get Job: %w", err)
	}

	// If Job is being deleted, wait for it to be removed
	if jobExists && job.DeletionTimestamp != nil {
		logger.Info("Job is being deleted, requeuing", "job", jobName)
		return ctrl.Result{RequeueAfter: requeueOnConflict}, nil
	}

	// Calculate current spec hash
	currentSpecHash := r.specHash(&probe)

	// Check if spec has changed by comparing with Job annotation (if job exists)
	// or with Probe status hash (if probe was previously checked)
	specChanged := false

	if jobExists {
		jobSpecHash, hasAnnotation := job.Annotations[specHashAnnotation]
		specChanged = !hasAnnotation || jobSpecHash != currentSpecHash
	}

	// Also check status hash if probe was previously checked and spec hasn't already been detected as changed
	if !specChanged && probe.Status.LastCheckTime != nil && probe.Status.SpecHash != currentSpecHash {
		specChanged = true
	}

	// If the spec has changed, delete the existing Job and reset the status
	if specChanged {
		logger.Info("Probe spec change detected, resetting probe", "probe", probe.Name,
			"oldHash", probe.Status.SpecHash, "newHash", currentSpecHash)

		probe.Status = checkv1alpha1.ProbeStatus{}
		if err := r.Status().Update(ctx, &probe); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reset status: %w", err)
		}

		// Refetch probe to get latest version after status update
		if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
			return ctrl.Result{}, err
		}

		// Delete the existing Job if it exists
		if jobExists {
			if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				if !errors.IsNotFound(err) {
					logger.Error(err, "Failed to delete Job due to spec change", "job", jobName)
					return ctrl.Result{}, err
				}
			}
			logger.Info("Deleted Job due to spec change", "job", jobName)
		}

		jobExists = false
		return ctrl.Result{RequeueAfter: requeueOnConflict}, nil
	}

	// Skip reconcile if already checked (run-once behavior)
	if probe.Status.LastCheckTime != nil {
		logger.Info("Skipping reconcile, probe already completed", "probe", probe.Name)
		return ctrl.Result{}, nil
	}

	// Create Job if it doesn't exist
	if !jobExists {
		// Check for creation timeout
		if time.Since(probe.CreationTimestamp.Time) > jobCreationTimeout {
			logger.Info("Job creation timed out", "probe", probe.Name, "timeout", jobCreationTimeout)

			probe.Status = checkv1alpha1.ProbeStatus{
				LastCheckTime: &metav1.Time{Time: time.Now()},
				State:         "Failed",
				Message:       fmt.Sprintf("Job creation timed out after %v", jobCreationTimeout),
			}
			if err := r.Status().Update(ctx, &probe); err != nil {
				return ctrl.Result{}, err
			}

			// Set metric to failed
			probeType := probe.Spec.Type
			probeVersion := probe.Spec.Version
			if probeVersion == "" {
				probeVersion = "unknown"
			}
			probeState.WithLabelValues(probe.Namespace, probe.Name, probeType, probeVersion, "0", probe.Spec.URL).Set(0)
			return ctrl.Result{}, nil
		}

		// Create new Job
		job = r.newJobForCR(&probe, currentSpecHash)

		// OwnerReference is already set in newJobForCR, but SetControllerReference
		// ensures the reference is properly configured as a controller owner
		if err := controllerutil.SetControllerReference(&probe, &job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Create(ctx, &job); err != nil {
			if errors.IsAlreadyExists(err) {
				logger.Info("Job creation conflict, requeuing", "job", jobName)
				return ctrl.Result{RequeueAfter: requeueOnConflict}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to create Job: %w", err)
		}

		logger.Info("Created Job for Probe", "job", jobName)
		return ctrl.Result{RequeueAfter: requeueAfterCreation}, nil
	}

	// Update status from Job if it exists
	if err := r.UpdateStatusFromJob(ctx, &probe, &job); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// specHash calculates a SHA256 hash of the probe spec
func (r *ProbeReconciler) specHash(p *checkv1alpha1.Probe) string {
	// Hash the entire spec as JSON for type-agnostic change detection
	specBytes, _ := json.Marshal(p.Spec)
	hash := sha256.Sum256(specBytes)
	return fmt.Sprintf("%x", hash)
}

// newJobForCR returns a new Job object for the Probe custom resource
func (r *ProbeReconciler) newJobForCR(p *checkv1alpha1.Probe, specHash string) batchv1.Job {
	backoffLimit := int32(3) // Allow up to 3 retries for transient failures
	ttl := jobTTLAfterFinished
	deadline := jobActiveDeadline

	executor := probes.Get(p.Spec.Type)
	container, _ := executor.BuildContainer(p.Spec)

	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name + "-job",
			Namespace: p.Namespace,
			Labels: map[string]string{
				"controller-uid": string(p.UID),
				"probe-name":     p.Name,
			},
			Annotations: map[string]string{
				specHashAnnotation: specHash,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(p, checkv1alpha1.GroupVersion.WithKind("Probe")),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:            []corev1.Container{container},
					RestartPolicy:         corev1.RestartPolicyNever,
					ActiveDeadlineSeconds: &deadline,
				},
			},
		},
	}
}

// UpdateStatusFromJob updates the Probe status based on the Job status
func (r *ProbeReconciler) UpdateStatusFromJob(ctx context.Context, p *checkv1alpha1.Probe, job *batchv1.Job) error {
	logger := log.FromContext(ctx)

	// Check if Job has completed
	completed := false
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) &&
			cond.Status == corev1.ConditionTrue {
			completed = true
			break
		}
	}

	if !completed {
		logger.V(1).Info("Job not completed yet", "job", job.Name)
		return nil
	}

	// Get all Pods created by the Job to count attempts
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name}); err != nil {
		logger.Error(err, "Failed to list Pods for Job", "job", job.Name)
		return err
	}

	// Safety check
	if len(podList.Items) == 0 {
		logger.Info("Job complete but Pod not found")
		return nil
	}

	totalPodAttempts := len(podList.Items)

	var probeResult probes.Result
	parseError := ""

	if totalPodAttempts > 0 {
		// Find the last Pod (most recent attempt)
		var latestPod *corev1.Pod
		for i := range podList.Items {
			pod := &podList.Items[i]
			if latestPod == nil || pod.CreationTimestamp.After(latestPod.CreationTimestamp.Time) {
				latestPod = pod
			}
		}

		if latestPod != nil {
			var message string
			if len(latestPod.Status.ContainerStatuses) > 0 {
				if state := latestPod.Status.ContainerStatuses[0].State.Terminated; state != nil {
					message = state.Message
					// If the container failed, capture the reason
					if state.ExitCode != 0 && state.Reason != "" {
						parseError = state.Reason
					}
				}
			}

			if message != "" {
				// Use executor to parse result
				executor := probes.Get(p.Spec.Type)
				if executor != nil {
					result, err := executor.ParseResult(message)
					if err == nil {
						probeResult = result
					} else {
						logger.Info("Failed to parse probe result", "error", err)
						parseError = err.Error()
					}
				}
			} else {
				logger.Info("No termination message found in Pod status")
			}
		}
	}

	// Build status from Job conditions
	now := metav1.NewTime(time.Now())
	status := checkv1alpha1.ProbeStatus{
		LastCheckTime: &now,
		SpecHash:      job.Annotations[specHashAnnotation],
		StatusCode:    probeResult.StatusCode,
		ResponseTime:  fmt.Sprintf("%.3f", probeResult.ResponseTime),
	}

	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			status.State = "Success"
			if totalPodAttempts > 1 {
				status.Message = fmt.Sprintf("Probe passed after %d attempts (Status: %d, Response Time: %.3fs)",
					totalPodAttempts, probeResult.StatusCode, probeResult.ResponseTime)
			} else {
				status.Message = fmt.Sprintf("Probe passed (Status: %d, Response Time: %.3fs)",
					probeResult.StatusCode, probeResult.ResponseTime)
			}
			logger.Info("Probe succeeded",
				"probe", p.Name,
				"attempts", totalPodAttempts,
				"statusCode", probeResult.StatusCode,
				"responseTime", probeResult.ResponseTime)
		} else if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			status.State = "Failed"

			if parseError != "" {
				status.Message = fmt.Sprintf("Probe failed after %d attempts: %s", totalPodAttempts, parseError)
			} else if probeResult.ErrorMessage != "" {
				status.Message = fmt.Sprintf("Probe failed after %d attempts: %s", totalPodAttempts, probeResult.ErrorMessage)
			} else if cond.Reason != "" {
				status.Message = fmt.Sprintf("Probe failed after %d attempts: %s", totalPodAttempts, cond.Reason)
			} else {
				status.Message = fmt.Sprintf("Probe failed after %d attempts", totalPodAttempts)
			}

			logger.Info("Probe failed",
				"probe", p.Name,
				"attempts", totalPodAttempts,
				"statusCode", probeResult.StatusCode,
				"error", parseError,
				"jobReason", cond.Reason)
		}
	}

	// Update Probe status
	p.Status = status
	if err := r.Status().Update(ctx, p); err != nil {
		logger.Error(err, "Failed to update Probe status", "probe", p.Name)
		return fmt.Errorf("failed to update status: %w", err)
	}

	// Update Prometheus metric
	statusCodeLabel := fmt.Sprintf("%d", probeResult.StatusCode)
	probeType := p.Spec.Type
	probeVersion := p.Spec.Version
	if probeVersion == "" {
		probeVersion = "unknown"
	}
	if status.State == "Success" {
		probeState.WithLabelValues(p.Namespace, p.Name, probeType, probeVersion, statusCodeLabel, p.Spec.URL).Set(1)
	} else {
		probeState.WithLabelValues(p.Namespace, p.Name, probeType, probeVersion, statusCodeLabel, p.Spec.URL).Set(0)
	}
	probeStatusCode.WithLabelValues(p.Namespace, p.Name, probeType, probeVersion, statusCodeLabel, p.Spec.URL).Set(float64(probeResult.StatusCode))
	probeResponseTime.WithLabelValues(p.Namespace, p.Name, probeType, probeVersion, statusCodeLabel, p.Spec.URL).Set(probeResult.ResponseTime)
	probePodAttempts.WithLabelValues(p.Namespace, p.Name, probeType, probeVersion, statusCodeLabel, p.Spec.URL).Set(float64(totalPodAttempts))

	logger.Info("Probe status updated",
		"probe", p.Name,
		"state", status.State,
		"attempts", totalPodAttempts,
		"statusCode", probeResult.StatusCode,
		"responseTime", probeResult.ResponseTime)

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ProbeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&checkv1alpha1.Probe{}).
		Owns(&batchv1.Job{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
