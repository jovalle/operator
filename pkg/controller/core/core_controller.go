package core

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	operatorv1alpha1 "github.com/tigera/operator/pkg/apis/operator/v1alpha1"
	"github.com/tigera/operator/pkg/render"

	configv1 "github.com/openshift/api/config/v1"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_core")
var openshiftEnv = "OPENSHIFT"
var defaultInstanceKey = client.ObjectKey{Name: "default"}
var openshiftNetworkConfig = "cluster"

// Add creates a new Core Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileCore{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("core-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Core
	err = c.Watch(&source.Kind{Type: &operatorv1alpha1.Core{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	if os.Getenv(openshiftEnv) == "true" {
		// Watch for openshift network configuration as well. If we're running in OpenShift, we need to
		// merge this configuration with our own and the write back the status object.
		err = c.Watch(&source.Kind{Type: &configv1.Network{}}, &handler.EnqueueRequestForObject{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	for _, t := range secondaryResources() {
		err = c.Watch(&source.Kind{Type: t}, &handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &operatorv1alpha1.Core{},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// secondaryResources returns a list of the secondary resources that this controller
// monitors for changes. Add resources here which correspond to the resources created by
// this controller.
func secondaryResources() []runtime.Object {
	return []runtime.Object{
		&apps.DaemonSet{},
		&rbacv1.ClusterRole{},
		&rbacv1.ClusterRoleBinding{},
		&v1.ServiceAccount{},
	}
}

var _ reconcile.Reconciler = &ReconcileCore{}

// ReconcileCore reconciles a Core object
type ReconcileCore struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

func fillDefaults(instance *operatorv1alpha1.Core) {
	if instance.Spec.Version == "" {
		instance.Spec.Version = "latest"
	}
	if len(instance.Spec.Registry) == 0 {
		instance.Spec.Registry = "docker.io/"
	}
	if !strings.HasSuffix(instance.Spec.Registry, "/") {
		instance.Spec.Registry = fmt.Sprintf("%s/", instance.Spec.Registry)
	}
	if len(instance.Spec.Variant) == 0 {
		instance.Spec.Variant = operatorv1alpha1.Calico
	}
	if len(instance.Spec.CNINetDir) == 0 {
		instance.Spec.CNINetDir = "/etc/cni/net.d"
	}
	if len(instance.Spec.CNIBinDir) == 0 {
		instance.Spec.CNIBinDir = "/opt/cni/bin"
	}
}

// Reconcile reads that state of the cluster for a Core object and makes changes based on the state read
// and what is in the Core.Spec. The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileCore) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.V(1).Info("Reconciling network installation")

	// Fetch the Core instance. We only support a single instance named "default".
	instance := &operatorv1alpha1.Core{}
	err := r.client.Get(context.TODO(), defaultInstanceKey, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("Network installation config not found")
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	fillDefaults(instance)
	reqLogger.Info("Loaded config", "config", instance)

	openshiftConfig := &configv1.Network{}
	if os.Getenv(openshiftEnv) == "true" {
		// If configured to run in openshift, then also fetch the openshift configuration API.
		reqLogger.V(1).Info("Querying for openshift network config")
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: openshiftNetworkConfig}, openshiftConfig)
		if err != nil {
			// Error reading the object - requeue the request.
			return reconcile.Result{}, err
		}

		// Use the openshift provided CIDRs.
		instance.Spec.IPPools = []operatorv1alpha1.IPPool{}
		for _, net := range openshiftConfig.Spec.ClusterNetwork {
			instance.Spec.IPPools = append(instance.Spec.IPPools, operatorv1alpha1.IPPool{CIDR: net.CIDR})
		}
	}

	// Render the desired objects based on our configuration.
	objs := renderObjects(instance)

	// Set Core instance as the owner and controller
	for _, obj := range objs {
		if err := controllerutil.SetControllerReference(instance, obj.(metav1.ObjectMetaAccessor).GetObjectMeta(), r.scheme); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Create the objects.
	for _, obj := range objs {
		logCtx := contextLoggerForResource(obj)
		var old runtime.Object = obj.DeepCopyObject()
		var key client.ObjectKey
		key, err = client.ObjectKeyFromObject(obj)
		if err != nil {
			return reconcile.Result{}, err
		}
		err = r.client.Get(context.TODO(), key, old)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				// Anything other than "Not found" we should retry.
				return reconcile.Result{}, err
			}
			// Otherwise, if it was not found, we should create it.
			logCtx.V(2).Info("Object does not exist", "error", err)
		} else {
			// Resource exists, skip it.
			// TODO: Reconcile any changes if the object doesn't match.
			logCtx.V(1).Info("Resource exists")
			continue
		}

		logCtx.Info("Creating new object")
		err = r.client.Create(context.TODO(), obj)
		if err != nil {
			// Hit an error creating object - we need to requeue.
			return reconcile.Result{}, err
		}
	}

	if os.Getenv(openshiftEnv) == "true" {
		// If configured to run in openshift, update the config status with the current state.
		reqLogger.V(1).Info("Updating openshift cluster network status")
		openshiftConfig.Status.ClusterNetwork = openshiftConfig.Spec.ClusterNetwork
		openshiftConfig.Status.ServiceNetwork = openshiftConfig.Spec.ServiceNetwork
		openshiftConfig.Status.ClusterNetworkMTU = 1440
		openshiftConfig.Status.NetworkType = "Calico"
		if err = r.client.Update(context.TODO(), openshiftConfig); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Created successfully - don't requeue
	reqLogger.V(1).Info("Finished reconciling network installation")
	return reconcile.Result{}, nil
}

func contextLoggerForResource(obj runtime.Object) logr.Logger {
	gvk := obj.GetObjectKind().GroupVersionKind()
	name := obj.(metav1.ObjectMetaAccessor).GetObjectMeta().GetName()
	namespace := obj.(metav1.ObjectMetaAccessor).GetObjectMeta().GetNamespace()
	return log.WithValues("Name", name, "Namespace", namespace, "Kind", gvk.Kind)
}

func renderObjects(cr *operatorv1alpha1.Core) []runtime.Object {
	var objs []runtime.Object

	objs = append(objs, render.Node(cr)...)
	objs = append(objs, render.Controllers(cr)...)
	return objs
}
