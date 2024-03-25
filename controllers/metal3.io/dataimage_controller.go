/*


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
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/metal3-io/baremetal-operator/pkg/provisioner"
	"github.com/metal3-io/baremetal-operator/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	dataImageRetryDelay  = time.Second * 60
	dataImageUpdateDelay = time.Second * 30
)

// DataImageReconciler reconciles a DataImage object.
type DataImageReconciler struct {
	client.Client
	Log                logr.Logger
	ProvisionerFactory provisioner.Factory
}

type rdiInfo struct {
	ctx     context.Context
	log     logr.Logger
	request ctrl.Request
	di      *metal3api.DataImage
	bmh     *metal3api.BareMetalHost
	events  []corev1.Event
}

// match the provisioner.EventPublisher interface.
func (info *rdiInfo) publishEvent(reason, message string) {
	t := metav1.Now()
	dataImageEvent := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: reason + "-",
			Namespace:    info.di.ObjectMeta.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "DataImage",
			Namespace:  info.di.Namespace,
			Name:       info.di.Name,
			UID:        info.di.UID,
			APIVersion: metal3api.GroupVersion.String(),
		},
		Reason:  reason,
		Message: message,
		Source: corev1.EventSource{
			Component: "metal3-dataimage-controller",
		},
		FirstTimestamp:      t,
		LastTimestamp:       t,
		Count:               1,
		Type:                corev1.EventTypeNormal,
		ReportingController: "metal3.io/dataimage-controller",
	}

	info.events = append(info.events, dataImageEvent)
}

//+kubebuilder:rbac:groups=metal3.io,resources=dataimages,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metal3.io,resources=dataimages/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=metal3.io,resources=dataimages/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *DataImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reqLogger := r.Log.WithValues("dataimage", req.NamespacedName)
	reqLogger.Info("start dataImage reconciliation V1")

	di := &metal3api.DataImage{}
	if err := r.Get(ctx, req.NamespacedName, di); err != nil {
		// The DataImage resource may have been deleted
		if k8serrors.IsNotFound(err) {
			reqLogger.Info("dataImage not found")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, errors.Wrap(err, "could not load dataImage")
	}

	bmh := &metal3api.BareMetalHost{}
	if err := r.Get(ctx, req.NamespacedName, bmh); err != nil {
		// There might not be any BareMetalHost for the DataImage
		if k8serrors.IsNotFound(err) {
			reqLogger.Info("bareMetalHost not found for the dataImage")
			return ctrl.Result{}, nil
		}

		// Error reading the object - requeue the request.
		return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, errors.Wrap(err, "could not load baremetalhost")
	}

	info := &rdiInfo{ctx: ctx, log: reqLogger, request: req, di: di, bmh: bmh}

	if hasDetachedAnnotation(bmh) {
		reqLogger.Info("the host is detached, not running reconciler")
		return ctrl.Result{Requeue: true, RequeueAfter: unmanagedRetryDelay}, nil
	}

	// TODO(hroyrh) : handle Paused annotation

	// Create a provisioner that can access Ironic API
	// prov, err := r.ProvisionerFactory.NewProvisioner(ctx, provisioner.BuildHostDataNoBMC(*bmh), info.publishEvent)
	prov, err := r.ProvisionerFactory.NewProvisioner(ctx, provisioner.BuildHostDataNoBMC(*bmh), info.publishEvent)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create provisioner")
	}

	ready, err := prov.TryInit()
	if err != nil || !ready {
		var msg string
		if err == nil {
			msg = "not ready"
		} else {
			msg = err.Error()
		}
		reqLogger.Info("provisioner is not ready", "Error", msg, "RequeueAfter", provisionerRetryDelay)
		return ctrl.Result{Requeue: true, RequeueAfter: provisionerRetryDelay}, nil
	}

	// Add finalizer for newly created DataImage
	if di.DeletionTimestamp.IsZero() && !utils.StringInList(di.Finalizers, metal3api.DataImageFinalizer) {
		reqLogger.Info("adding finalizer")
		di.Finalizers = append(di.Finalizers, metal3api.DataImageFinalizer)

		err := r.Update(ctx, di)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update resource after add finalizer")
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch the latest status of DataImage from Node
	dataImageStatus, err := prov.GetDataImageStatus()
	if err != nil {
		reqLogger.Info("Failed to get current dataimage status", "Error", err)
		return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, fmt.Errorf("failed to get latest status, Error = %w", err)
	}
	// Copy the fetched status into the resource status
	dataImageStatus.DeepCopyInto(&di.Status)

	// Remove finalizer if DataImage has been requested for deletion and
	// there is no attached image, else wait for the detachment.
	if !di.DeletionTimestamp.IsZero() {
		reqLogger.Info("cleaning up deleted dataImage resource")

		dataImageAttachedURL := di.Status.AttachedImage.URL

		if dataImageAttachedURL != "" {
			reqLogger.Info("Wait for DataImage to detach before removing finalizer, requeueing")
			return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, nil
		}

		di.Finalizers = utils.FilterStringFromList(
			di.Finalizers, metal3api.DataImageFinalizer)

		if err := r.Update(ctx, di); err != nil {
			return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, errors.Wrap(err, "failed to update resource after remove finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Update the latest status fetched from the Node
	if err := r.updateStatus(info); err != nil {
		return ctrl.Result{Requeue: true, RequeueAfter: dataImageRetryDelay}, errors.Wrap(err, "failed to update resource statu")
	}

	for _, e := range info.events {
		r.publishEvent(ctx, req, e)
	}

	return ctrl.Result{}, nil
}

// Update the DataImage status after fetching current status from provisioner.
func (r *DataImageReconciler) updateStatus(info *rdiInfo) (err error) {
	dataImage := info.di

	if err := r.Status().Update(info.ctx, dataImage); err != nil {
		return errors.Wrap(err, "Failed to update DataImage status")
	}
	info.log.Info("Updating DataImage Status", "Updated DataImage is", dataImage)

	return nil
}

// Publish reconciler events.
func (r *DataImageReconciler) publishEvent(ctx context.Context, request ctrl.Request, event corev1.Event) {
	reqLogger := r.Log.WithValues("dataimage", request.NamespacedName)
	reqLogger.Info("publishing event", "reason", event.Reason, "message", event.Message)
	err := r.Create(ctx, &event)
	if err != nil {
		reqLogger.Info("failed to record event, ignoring",
			"reason", event.Reason, "message", event.Message, "error", err)
	}
}

// Update events.
func (r *DataImageReconciler) updateEventHandler(e event.UpdateEvent) bool {
	r.Log.Info("dataimage in event handler")

	// If the update increased the resource Generation then let's process it
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		r.Log.Info("returning true as generation changed from event handler")
		return true
	}

	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *DataImageReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconcile int) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&metal3api.DataImage{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconcile}).
		WithEventFilter(
			predicate.Funcs{
				UpdateFunc: r.updateEventHandler,
			}).
		Complete(r)
}
