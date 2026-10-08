// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/microvmpreboot"
	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
)

const workerPoolFieldOwner = "workerpool-controller"

type WorkerPoolReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	OTelEndpoint string
	// OTelMetricExportInterval is the OTEL_METRIC_EXPORT_INTERVAL propagated to
	// ateom pods. Empty keeps the SDK's default.
	OTelMetricExportInterval string
	// OTelMetricExportTimeout is the OTEL_METRIC_EXPORT_TIMEOUT propagated to
	// ateom pods. Empty keeps the SDK's default.
	OTelMetricExportTimeout string
	// OTelTracesSampler is the OTEL_TRACES_SAMPLER propagated to ateom pods.
	// Empty keeps the ateom binary's default.
	OTelTracesSampler string
	// OTelTracesSamplerArg is the OTEL_TRACES_SAMPLER_ARG propagated to ateom
	// pods. Ignored unless OTelTracesSampler is set.
	OTelTracesSamplerArg string
	// SystemNamespace is the namespace substrate's control plane runs in, and
	// AteletServiceAccount / RouterServiceAccount are the ServiceAccounts those
	// components run as. Together they name the SPIFFE identities that atunnel
	// authenticates inside each worker, which is why the ServiceAccount names
	// are configuration and not constants: a deployment that prefixes
	// resource names changes them.
	SystemNamespace      string
	AteletServiceAccount string
	RouterServiceAccount string

	desiredWorkers metric.Int64ObservableUpDownCounter
	readyWorkers   metric.Int64ObservableUpDownCounter
}

//+kubebuilder:rbac:groups=ate.dev,resources=workerpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ate.dev,resources=sandboxconfigs,verbs=get;list;watch
//+kubebuilder:rbac:groups=ate.dev,resources=workerpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ate.dev,resources=workerpools/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WorkerPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch worker pool
	wp := &atev1alpha1.WorkerPool{}
	if err := r.Get(ctx, req.NamespacedName, wp); err != nil {
		if k8errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get worker pool %q: %w", req.NamespacedName, err)
	}

	// Handle deletion
	if !wp.GetDeletionTimestamp().IsZero() {
		log.Info("WorkerPool is being deleted")
		return ctrl.Result{}, nil
	}

	if err := r.reconcileWorkerPool(ctx, wp); err != nil {
		log.Error(err, "Failed to reconcile worker pool")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *WorkerPoolReconciler) reconcileWorkerPool(ctx context.Context, wp *atev1alpha1.WorkerPool) error {
	log := log.FromContext(ctx)
	log.Info("Reconciling worker pool")

	if err := r.applyDeployment(ctx, wp); err != nil {
		return err
	}

	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: wp.Name, Namespace: wp.Namespace}, dep); err != nil {
		if k8errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	return r.syncStatus(ctx, wp, dep)
}

func (r *WorkerPoolReconciler) applyDeployment(ctx context.Context, wp *atev1alpha1.WorkerPool) error {
	depAC := buildDeploymentApplyConfig(wp, ateomOTelSettings{
		Endpoint:             r.OTelEndpoint,
		MetricExportInterval: r.OTelMetricExportInterval,
		MetricExportTimeout:  r.OTelMetricExportTimeout,
		TracesSampler:        r.OTelTracesSampler,
		TracesSamplerArg:     r.OTelTracesSamplerArg,
	}, r.SystemNamespace, r.AteletServiceAccount, r.RouterServiceAccount)
	if wp.Spec.MicroVMPreboot != nil {
		config := &atev1alpha1.SandboxConfig{}
		if err := r.Get(ctx, types.NamespacedName{Name: wp.Spec.MicroVMPreboot.SandboxConfigName}, config); err != nil {
			return fmt.Errorf("preboot SandboxConfig: %w", err)
		}
		env, err := workerPrebootEnv(wp, config)
		if err != nil {
			return err
		}
		depAC.Spec.Template.Spec.Containers[0].WithEnv(env)
	}
	if err := r.Apply(ctx, depAC, client.FieldOwner(workerPoolFieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("failed to apply Deployment: %w", err)
	}
	return nil
}

func (r *WorkerPoolReconciler) syncStatus(ctx context.Context, wp *atev1alpha1.WorkerPool, dep *appsv1.Deployment) error {
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return fmt.Errorf("failed to convert Deployment selector: %w", err)
	}

	want := atev1alpha1.WorkerPoolStatus{
		Replicas:      dep.Status.Replicas,
		ReadyReplicas: dep.Status.ReadyReplicas,
		Selector:      selector.String(),
	}
	if equality.Semantic.DeepEqual(wp.Status, want) {
		return nil
	}

	wp.Status = want
	if err := r.Status().Update(ctx, wp); err != nil {
		return fmt.Errorf("failed to update WorkerPool status: %w", err)
	}

	return nil
}

func workerPrebootEnv(wp *atev1alpha1.WorkerPool, sandbox *atev1alpha1.SandboxConfig) (*corev1ac.EnvVarApplyConfiguration, error) {
	if wp.Spec.SandboxClass != atev1alpha1.SandboxClassMicroVM || sandbox.Spec.SandboxClass != atev1alpha1.SandboxClassMicroVM {
		return nil, fmt.Errorf("preboot requires a microvm worker and SandboxConfig")
	}
	config := microvmpreboot.Config{Spec: *wp.Spec.MicroVMPreboot, Assets: map[string]map[string]string{}}
	for arch, assets := range sandbox.Spec.Assets {
		config.Assets[arch] = map[string]string{}
		for name, asset := range assets {
			config.Assets[arch][name] = asset.SHA256
		}
		if _, err := config.Paths(arch); err != nil {
			return nil, err
		}
	}
	if len(config.Assets) == 0 || wp.Spec.Template == nil || wp.Spec.Template.Resources == nil {
		return nil, fmt.Errorf("preboot requires runtime assets and explicit worker CPU/memory limits")
	}
	limits := wp.Spec.Template.Resources.Limits
	cpu, memory := limits["cpu"], limits["memory"]
	if cpu.MilliValue() < config.Spec.CPUMilli+microvmpreboot.BackgroundCPUMilli || memory.Value() < config.ReservedMemoryBytes()+int64(config.Spec.MemoryMiB)*1024*1024 {
		return nil, fmt.Errorf("worker limits must cover idle preboot VMs, one matching Actor, and 250m background CPU")
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return corev1ac.EnvVar().WithName(microvmpreboot.EnvName).WithValue(string(data)), nil
}

func (r *WorkerPoolReconciler) prebootConfigUsers(ctx context.Context, object client.Object) []reconcile.Request {
	var pools atev1alpha1.WorkerPoolList
	if err := r.List(ctx, &pools); err != nil {
		log.FromContext(ctx).Error(err, "List preboot WorkerPools")
		return nil
	}
	var requests []reconcile.Request
	for _, pool := range pools.Items {
		if pool.Spec.MicroVMPreboot != nil && pool.Spec.MicroVMPreboot.SandboxConfigName == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: pool.Name, Namespace: pool.Namespace}})
		}
	}
	return requests
}

// InitMetrics initializes the OpenTelemetry instruments for ate.workerpool.desired_workers
// and ate.workerpool.ready_workers and registers the asynchronous callback.
func (r *WorkerPoolReconciler) InitMetrics(meter metric.Meter) error {
	desiredWorkers, err := meter.Int64ObservableUpDownCounter(
		"ate.workerpool.desired_workers",
		metric.WithUnit("{worker}"),
		metric.WithDescription("number of worker pods requested for a WorkerPool (spec.replicas)"),
	)
	if err != nil {
		return fmt.Errorf("create ate.workerpool.desired_workers instrument: %w", err)
	}
	r.desiredWorkers = desiredWorkers

	readyWorkers, err := meter.Int64ObservableUpDownCounter(
		"ate.workerpool.ready_workers",
		metric.WithUnit("{worker}"),
		metric.WithDescription("number of worker pods currently ready for a WorkerPool (status.readyReplicas)"),
	)
	if err != nil {
		return fmt.Errorf("create ate.workerpool.ready_workers instrument: %w", err)
	}
	r.readyWorkers = readyWorkers

	_, err = meter.RegisterCallback(
		func(ctx context.Context, obs metric.Observer) error {
			var list atev1alpha1.WorkerPoolList
			if err := r.List(ctx, &list); err != nil {
				log.FromContext(ctx).Error(err, "failed to list worker pools to observe ate.workerpool.desired_workers and ate.workerpool.ready_workers")
				return nil
			}
			for _, wp := range list.Items {
				attrs := metric.WithAttributes(
					ateattr.WorkerPoolNamespaceKey.String(wp.Namespace),
					ateattr.WorkerPoolNameKey.String(wp.Name),
				)
				obs.ObserveInt64(r.desiredWorkers, int64(wp.Spec.Replicas), attrs)
				obs.ObserveInt64(r.readyWorkers, int64(wp.Status.ReadyReplicas), attrs)
			}
			return nil
		},
		r.desiredWorkers,
		r.readyWorkers,
	)
	if err != nil {
		return fmt.Errorf("register workerpool metrics callback: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkerPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := r.InitMetrics(otel.Meter("atecontroller")); err != nil {
		return fmt.Errorf("failed to initialize workerpool metrics: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&atev1alpha1.WorkerPool{}).
		Owns(&appsv1.Deployment{}).
		Watches(&atev1alpha1.SandboxConfig{}, handler.EnqueueRequestsFromMapFunc(r.prebootConfigUsers)).
		Complete(r)
}
