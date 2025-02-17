/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"k8s.io/utils/pointer"

	viewv1 "github.com/zoetrope/markdown-view/api/v1"
	"github.com/zoetrope/markdown-view/pkg/constants"
	"github.com/zoetrope/markdown-view/pkg/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1apply "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// MarkdownViewReconciler reconciles a MarkdownView object
type MarkdownViewReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//! [rbac]
//+kubebuilder:rbac:groups=view.zoetrope.github.io,resources=markdownviews,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=view.zoetrope.github.io,resources=markdownviews/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=view.zoetrope.github.io,resources=markdownviews/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch
//! [rbac]

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the MarkdownView object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
//! [reconcile]
func (r *MarkdownViewReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mdView viewv1.MarkdownView
	err := r.Get(ctx, req.NamespacedName, &mdView)
	if errors.IsNotFound(err) {
		r.removeMetrics(mdView)
		return ctrl.Result{}, nil
	}
	if err != nil {
		logger.Error(err, "unable to get MarkdownView", "name", req.NamespacedName)
		return ctrl.Result{}, err
	}

	if !mdView.ObjectMeta.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	err = r.reconcileConfigMap(ctx, mdView)
	if err != nil {
		return ctrl.Result{}, err
	}
	err = r.reconcileDeployment(ctx, mdView)
	if err != nil {
		return ctrl.Result{}, err
	}
	err = r.reconcileService(ctx, mdView)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, mdView)
}

//! [reconcile]

//! [reconcile-configmap]
func (r *MarkdownViewReconciler) reconcileConfigMap(ctx context.Context, mdView viewv1.MarkdownView) error {
	logger := log.FromContext(ctx)

	cm := &corev1.ConfigMap{}
	cm.SetNamespace(mdView.Namespace)
	cm.SetName("markdowns-" + mdView.Name)

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		for name, content := range mdView.Spec.Markdowns {
			cm.Data[name] = content
		}
		return ctrl.SetControllerReference(&mdView, cm, r.Scheme)
	})

	if err != nil {
		logger.Error(err, "unable to create or update ConfigMap")
		return err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("reconcile ConfigMap successfully", "op", op)
	}
	return nil
}

//! [reconcile-configmap]

func (r *MarkdownViewReconciler) reconcileDeployment(ctx context.Context, mdView viewv1.MarkdownView) error {
	logger := log.FromContext(ctx)

	depName := "viewer-" + mdView.Name
	viewerImage := constants.DefaultViewerImage
	if len(mdView.Spec.ViewerImage) != 0 {
		viewerImage = mdView.Spec.ViewerImage
	}

	owner, err := ownerRef(mdView, r.Scheme)
	if err != nil {
		return err
	}

	dep := appsv1apply.Deployment(depName, mdView.Namespace).
		WithLabels(labelSet(mdView)).
		WithOwnerReferences(owner).
		WithSpec(appsv1apply.DeploymentSpec().
			WithReplicas(mdView.Spec.Replicas).
			WithSelector(metav1apply.LabelSelector().WithMatchLabels(labelSet(mdView))).
			WithTemplate(corev1apply.PodTemplateSpec().
				WithLabels(labelSet(mdView)).
				WithSpec(corev1apply.PodSpec().
					WithContainers(corev1apply.Container().
						WithName(constants.ViewerName).
						WithImage(viewerImage).
						WithImagePullPolicy(corev1.PullIfNotPresent).
						WithCommand("mdbook").
						WithArgs("serve", "--hostname", "0.0.0.0").
						WithVolumeMounts(corev1apply.VolumeMount().
							WithName("markdowns").
							WithMountPath("/book/src"),
						).
						WithPorts(corev1apply.ContainerPort().
							WithName("http").
							WithProtocol(corev1.ProtocolTCP).
							WithContainerPort(3000),
						).
						WithLivenessProbe(corev1apply.Probe().
							WithHTTPGet(corev1apply.HTTPGetAction().
								WithPort(intstr.FromString("http")).
								WithPath("/").
								WithScheme(corev1.URISchemeHTTP),
							),
						).
						WithReadinessProbe(corev1apply.Probe().
							WithHTTPGet(corev1apply.HTTPGetAction().
								WithPort(intstr.FromString("http")).
								WithPath("/").
								WithScheme(corev1.URISchemeHTTP),
							),
						),
					).
					WithVolumes(corev1apply.Volume().
						WithName("markdowns").
						WithConfigMap(corev1apply.ConfigMapVolumeSource().
							WithName("markdowns-" + mdView.Name),
						),
					),
				),
			),
		)

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		return err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}

	var current appsv1.Deployment
	err = r.Get(ctx, client.ObjectKey{Namespace: mdView.Namespace, Name: depName}, &current)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	currApplyConfig, err := appsv1apply.ExtractDeployment(&current, constants.ControllerName)
	if err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(dep, currApplyConfig) {
		return nil
	}

	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: constants.ControllerName,
		Force: pointer.Bool(true),
	})

	if err != nil {
		logger.Error(err, "unable to create or update Deployment")
		return err
	}
	logger.Info("reconcile Deployment successfully", "name", mdView.Name)
	return nil
}

//! [reconcile-service]
func (r *MarkdownViewReconciler) reconcileService(ctx context.Context, mdView viewv1.MarkdownView) error {
	logger := log.FromContext(ctx)
	svcName := "viewer-" + mdView.Name

	owner, err := ownerRef(mdView, r.Scheme)
	if err != nil {
		return err
	}

	svc := corev1apply.Service(svcName, mdView.Namespace).
		WithLabels(labelSet(mdView)).
		WithOwnerReferences(owner).
		WithSpec(corev1apply.ServiceSpec().
			WithSelector(labelSet(mdView)).
			WithType(corev1.ServiceTypeClusterIP).
			WithPorts(corev1apply.ServicePort().
				WithProtocol(corev1.ProtocolTCP).
				WithPort(80).
				WithTargetPort(intstr.FromInt(3000)),
			),
		)

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(svc)
	if err != nil {
		return err
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}

	var current corev1.Service
	err = r.Get(ctx, client.ObjectKey{Namespace: mdView.Namespace, Name: svcName}, &current)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	currApplyConfig, err := corev1apply.ExtractService(&current, constants.ControllerName)
	if err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(svc, currApplyConfig) {
		return nil
	}

	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: constants.ControllerName,
		Force: pointer.Bool(true),
	})
	if err != nil {
		logger.Error(err, "unable to create or update Service")
		return err
	}

	logger.Info("reconcile Service successfully", "name", mdView.Name)
	return nil
}

//! [reconcile-service]

//! [update-status]
func (r *MarkdownViewReconciler) updateStatus(ctx context.Context, mdView viewv1.MarkdownView) (ctrl.Result, error) {
	var dep appsv1.Deployment
	err := r.Get(ctx, client.ObjectKey{Namespace: mdView.Namespace, Name: "viewer-" + mdView.Name}, &dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	var status viewv1.MarkdownViewStatus
	if dep.Status.AvailableReplicas == 0 {
		status = viewv1.MarkdownViewNotReady
	} else if dep.Status.AvailableReplicas == mdView.Spec.Replicas {
		status = viewv1.MarkdownViewHealthy
	} else {
		status = viewv1.MarkdownViewAvailable
	}

	if mdView.Status != status {
		mdView.Status = status
		r.setMetrics(mdView)

		r.Recorder.Event(&mdView, corev1.EventTypeNormal, "Updated", fmt.Sprintf("MarkdownView(%s:%s) updated: %s", mdView.Namespace, mdView.Name, mdView.Status))

		err = r.Status().Update(ctx, &mdView)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	if mdView.Status != viewv1.MarkdownViewHealthy {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

//! [update-status]

//! [set-metrics]
func (r *MarkdownViewReconciler) setMetrics(mdView viewv1.MarkdownView) {
	switch mdView.Status {
	case viewv1.MarkdownViewNotReady:
		metrics.NotReadyVec.WithLabelValues(mdView.Name, mdView.Name).Set(1)
		metrics.AvailableVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
		metrics.HealthyVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
	case viewv1.MarkdownViewAvailable:
		metrics.NotReadyVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
		metrics.AvailableVec.WithLabelValues(mdView.Name, mdView.Name).Set(1)
		metrics.HealthyVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
	case viewv1.MarkdownViewHealthy:
		metrics.NotReadyVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
		metrics.AvailableVec.WithLabelValues(mdView.Name, mdView.Name).Set(0)
		metrics.HealthyVec.WithLabelValues(mdView.Name, mdView.Name).Set(1)
	}
}

//! [set-metrics]

//! [remove-metrics]
func (r *MarkdownViewReconciler) removeMetrics(mdView viewv1.MarkdownView) {
	metrics.NotReadyVec.DeleteLabelValues(mdView.Name, mdView.Name)
	metrics.AvailableVec.DeleteLabelValues(mdView.Name, mdView.Name)
	metrics.HealthyVec.DeleteLabelValues(mdView.Name, mdView.Name)
}

//! [remove-metrics]

func ownerRef(mdView viewv1.MarkdownView, scheme *runtime.Scheme) (*metav1apply.OwnerReferenceApplyConfiguration, error) {
	gvk, err := apiutil.GVKForObject(&mdView, scheme)
	if err != nil {
		return nil, err
	}
	ref := metav1apply.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(mdView.Name).
		WithUID(mdView.GetUID()).
		WithBlockOwnerDeletion(true).
		WithController(true)
	return ref, nil
}

func labelSet(mdView viewv1.MarkdownView) map[string]string {
	labels := map[string]string{
		constants.LabelAppName:      constants.ViewerName,
		constants.LabelAppInstance:  mdView.Name,
		constants.LabelAppCreatedBy: constants.ControllerName,
	}
	return labels
}

// SetupWithManager sets up the controller with the Manager.
//! [managedby]
func (r *MarkdownViewReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&viewv1.MarkdownView{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

//! [managedby]
