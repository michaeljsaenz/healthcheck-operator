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
	"fmt"
	"strings"
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

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics
var (
	probeStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_healthy",
			Help: "Health status of the probe CRs (1 = healthy, 0 = unhealthy)",
		},
		[]string{"namespace", "name"},
	)
)

func init() {
	metrics.Registry.MustRegister(probeStatus)
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Probe object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *ProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Declare a Probe instance
	var probe checkv1alpha1.Probe
	// Get the Probe instance
	if err := r.Get(ctx, req.NamespacedName, &probe); err != nil {
		if errors.IsNotFound(err) {
			// The Probe resource may have been deleted after the reconcile request.
			// In this case, we don't need to requeue.
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, err
	}

	// Check if the Job already exists
	jobName := probe.Name + "-job"
	//
	var job batchv1.Job
	jobExists := true
	// Try to get the Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: probe.Namespace}, &job)
	if err != nil && errors.IsNotFound(err) {
		jobExists = false
	} else if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("failed to get Job: %w", err)
	}

	if err == nil && job.DeletionTimestamp != nil {
		log.Info("Job is being deleted, requeuing")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// Determine if the spec has changed by comparing the URL in the Probe spec with the Job's container args
	// This assumes the Job has only one container and the curl command is in the first arg
	// Adjust this logic if your Job spec is more complex
	// or if you want to track more fields for changes
	// If the Job does not exist, we consider the spec as changed to trigger creation
	// If the Job exists but the URL has changed, we also consider it as changed
	// This will lead to deletion of the old Job and creation of a new one
	// Note: This is a simple comparison; for more complex specs, consider using a hash or checksum
	// of the relevant fields in the Probe spec and store it in an annotation on the Job
	// for more robust change detection
	// Here, we only check the URL, but you can extend this to other fields as needed
	specChanged := jobExists && len(job.Spec.Template.Spec.Containers) > 0 &&
		strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ") != fmt.Sprintf("-f -s --head %s", probe.Spec.URL)

	// If the spec has changed, delete the existing Job and reset the status
	if specChanged {
		probe.Status = checkv1alpha1.ProbeStatus{}
		// Update the status to clear LastCheckTime
		if err := r.Status().Update(ctx, &probe); err != nil {
			return ctrl.Result{}, err
		}
		// Delete the existing Job
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if errors.IsNotFound(err) {
				log.Info("Job already deleted due to spec change", "job", jobName)
			} else {
				log.Error(err, "Failed to delete Job due to spec change", "job", jobName)
				return ctrl.Result{}, err
			}
		}
		jobExists = false
		log.Info("Deleted Job due to spec change", "job", jobName)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// If probe has already been checked (either success or failure), skip reconciliation
	if probe.Status.LastCheckTime != nil && !specChanged {
		log.Info("Skipping reconcile, probe already completed", "probe", probe.Name, "healthy", probe.Status.Healthy)
		return ctrl.Result{}, nil
	}

	if !jobExists {
		if time.Since(probe.CreationTimestamp.Time) > 5*time.Minute {
			probe.Status = checkv1alpha1.ProbeStatus{
				LastCheckTime: &metav1.Time{Time: time.Now()},
				Healthy:       false,
				Message:       "Job creation timed out",
			}
			if err := r.Status().Update(ctx, &probe); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		job = r.newJobForCR(&probe)
		if err := controllerutil.SetControllerReference(&probe, &job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &job); err != nil {
			if errors.IsAlreadyExists(err) {
				log.Info("Job creation conflict, requeuing", "job", jobName)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}
		log.Info("Created Job for Probe", "job", jobName)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.UpdateStatusFromJob(ctx, &probe, &job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// newJobForCR returns a curl job object for the Probe CR
func (r *ProbeReconciler) newJobForCR(p *checkv1alpha1.Probe) batchv1.Job {
	return batchv1.Job{
		ObjectMeta: ctrl.ObjectMeta{
			Name:      p.Name + "-job",
			Namespace: p.Namespace,
			Labels:    map[string]string{"controller-uid": string(p.UID)},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(p, checkv1alpha1.GroupVersion.WithKind("Probe")),
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "probe",
							Image: "curlimages/curl:8.10.1",
							Args:  []string{"-f", "-s", "--head", p.Spec.URL},
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
	}
}

// UpdateStatusFromJob updates the Probe status based on the Job status
func (r *ProbeReconciler) UpdateStatusFromJob(ctx context.Context, p *checkv1alpha1.Probe, job *batchv1.Job) error {
	completed := false
	for _, cond := range job.Status.Conditions {
		if (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) && cond.Status == corev1.ConditionTrue {
			completed = true
			break
		}
	}

	if !completed {
		log.Log.Info("Job not completed yet", "job", job.Name)
		return nil
	}

	now := metav1.NewTime(time.Now())
	status := checkv1alpha1.ProbeStatus{
		LastCheckTime: &now,
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			status.Healthy = true
			status.Message = "Probe passed"
		} else if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			status.Healthy = false
			status.Message = "Probe failed"
		}
	}

	p.Status = status
	if err := r.Status().Update(ctx, p); err != nil {
		log.Log.Error(err, "Failed to update Probe status", "probe", p.Name)
		return err
	}

	probeStatus.WithLabelValues(p.Namespace, p.Name).Set(1)
	if !status.Healthy {
		probeStatus.WithLabelValues(p.Namespace, p.Name).Set(0)
	}

	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		log.Log.Error(err, "Failed to delete Job after completion", "job", job.Name)
		return err
	}
	log.Log.Info("Deleted Job", "job", job.Name)
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
