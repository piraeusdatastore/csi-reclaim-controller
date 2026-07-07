// Package controller implements a Kubernetes controller that reclaims the
// backing storage of PersistentVolumes that use "reclaimPolicy: Retain".
//
// With the Retain policy, deleting a PV (whether manually or otherwise) leaves
// the backing volume in the storage backend untouched, which is intentional per
// the Kubernetes storage model. This controller offers an opt-in alternative:
// for every Retain PV that belongs to a given CSI driver, it places a finalizer
// and, once such a PV is deleted, issues DeleteVolume() against the driver
// before releasing the finalizer. The result is that deleting a Retain PV of
// that driver also frees the underlying volume.
//
// Deploying the controller is itself the opt-in. An individual PV can be
// excluded with the skip annotation, which also lets another controller (or an
// admin) delete-and-recreate a PV in place without its volume being reclaimed.
package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// annDeletionSecretName and annDeletionSecretNamespace are written onto the
	// PV by external-provisioner and point at the secret to pass to
	// DeleteVolume. We reuse them so credential handling matches the sidecar
	// that created the volume.
	annDeletionSecretName      = "volume.kubernetes.io/provisioner-deletion-secret-name"
	annDeletionSecretNamespace = "volume.kubernetes.io/provisioner-deletion-secret-namespace"

	reasonReclaiming    = "Reclaiming"
	reasonReclaimed     = "Reclaimed"
	reasonReclaimFailed = "ReclaimFailed"
	reasonMissingHandle = "MissingVolumeHandle"
)

// VolumeDeleter abstracts the CSI DeleteVolume call so the controller can be
// unit-tested without a live gRPC endpoint.
type VolumeDeleter interface {
	DeleteVolume(ctx context.Context, volumeID string, secrets map[string]string) error
}

// Controller watches PersistentVolumes and reclaims the backing storage of
// deleted Retain volumes that belong to a single CSI driver.
type Controller struct {
	kubeClient kubernetes.Interface
	pvLister   corelisters.PersistentVolumeLister
	pvSynced   cache.InformerSynced
	queue      workqueue.TypedRateLimitingInterface[string]
	recorder   record.EventRecorder

	deleter    VolumeDeleter
	driverName string
	finalizer  string

	// skipAnnotation, when set to a true value on a PV, temporarily excludes it
	// from management: the controller neither reclaims its backing volume nor
	// keeps its finalizer. It lets other controllers safely delete-and-recreate
	// a PV in place without triggering DeleteVolume — in particular
	// linstor-affinity-controller, which patches a PV to Retain, deletes it, and
	// re-applies it (same volumeHandle) to update node affinity. Without this
	// marker that recreate would look like a user deleting a Retain PV and would
	// destroy a live volume. It defaults to "<driver-name>/skip-reclaim".
	skipAnnotation string
}

// New creates a Controller. A PV is managed when spec.csi.driver equals
// driverName, its reclaim policy is Retain, and it is not marked with
// skipAnnotation. finalizer is the finalizer key placed on managed PVs.
func New(
	kubeClient kubernetes.Interface,
	pvInformer informers.SharedInformerFactory,
	deleter VolumeDeleter,
	driverName string,
	finalizer string,
	skipAnnotation string,
	recorder record.EventRecorder,
) (*Controller, error) {
	pvs := pvInformer.Core().V1().PersistentVolumes()
	c := &Controller{
		kubeClient: kubeClient,
		pvLister:   pvs.Lister(),
		pvSynced:   pvs.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "csi-reclaim-controller"},
		),
		recorder:       recorder,
		deleter:        deleter,
		driverName:     driverName,
		finalizer:      finalizer,
		skipAnnotation: skipAnnotation,
	}

	if _, err := pvs.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueue,
		UpdateFunc: func(_, newObj interface{}) {
			c.enqueue(newObj)
		},
	}); err != nil {
		return nil, fmt.Errorf("register PersistentVolume event handler: %w", err)
	}
	return c, nil
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.ErrorS(err, "Failed to derive key for PersistentVolume")
		return
	}
	c.queue.Add(key)
}

// Run starts the worker loop and blocks until ctx is cancelled.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	klog.InfoS("Starting reclaim controller", "driver", c.driverName, "finalizer", c.finalizer)
	if !cache.WaitForCacheSync(ctx.Done(), c.pvSynced) {
		return fmt.Errorf("failed to sync PersistentVolume cache")
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
	klog.InfoS("Reclaim controller running", "workers", workers)

	<-ctx.Done()
	klog.InfoS("Shutting down reclaim controller")
	return nil
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

func (c *Controller) processNextItem(ctx context.Context) bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.syncPV(ctx, key); err != nil {
		klog.ErrorS(err, "Error reconciling PersistentVolume, will retry", "pv", key)
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// syncPV is the reconcile loop for a single PersistentVolume.
func (c *Controller) syncPV(ctx context.Context, key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	pv, err := c.pvLister.Get(name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	managed := c.isManaged(pv)
	hasFinalizer := slices.Contains(pv.Finalizers, c.finalizer)

	if pv.DeletionTimestamp != nil {
		if !hasFinalizer {
			return nil
		}
		// The reclaim policy or driver may have changed away from what we
		// manage after we placed the finalizer (e.g. Retain -> Delete, now
		// owned by external-provisioner). Release without touching storage.
		if !managed {
			klog.InfoS("PV no longer managed while terminating, releasing finalizer",
				"pv", name)
			return c.removeFinalizer(ctx, pv)
		}
		return c.reclaim(ctx, pv)
	}

	// Not being deleted: keep the finalizer in sync with whether we manage it.
	if managed {
		if !hasFinalizer {
			return c.addFinalizer(ctx, pv)
		}
		return nil
	}
	if hasFinalizer {
		return c.removeFinalizer(ctx, pv)
	}
	return nil
}

// isManaged reports whether pv is a Retain volume provisioned by our driver that
// is not excluded by the skip annotation.
func (c *Controller) isManaged(pv *corev1.PersistentVolume) bool {
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != c.driverName {
		return false
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return false
	}
	// The skip annotation excludes a PV from management: it lets another
	// controller (or an admin) delete-and-recreate this PV in place — e.g. an
	// affinity update — without its backing volume being reclaimed. It is
	// evaluated here (not on the informer) so that a PV which is unmarked while
	// terminating is still observed and its finalizer released, rather than
	// being stranded. See the skipAnnotation field.
	if skip, err := strconv.ParseBool(pv.Annotations[c.skipAnnotation]); err == nil && skip {
		return false
	}
	return true
}

// reclaim issues DeleteVolume for a terminating, managed PV and then releases
// the finalizer so the PV object can be removed.
func (c *Controller) reclaim(ctx context.Context, pv *corev1.PersistentVolume) error {
	// A still-bound PV must not have its storage deleted: a workload may still
	// be using it. Kubernetes keeps such a PV alive via the pv-protection
	// finalizer until its claim is gone; we simply wait for the phase to leave
	// Bound, which triggers another reconcile.
	if pv.Status.Phase == corev1.VolumeBound {
		klog.InfoS("PV is terminating but still Bound, deferring reclaim", "pv", pv.Name)
		return nil
	}

	volumeID := pv.Spec.CSI.VolumeHandle
	if volumeID == "" {
		// Nothing actionable; do not block PV removal forever, but make the
		// leak loud so an operator can clean up the backend manually.
		klog.ErrorS(nil, "Managed PV has no volumeHandle, cannot reclaim backing volume",
			"pv", pv.Name)
		c.eventf(pv, corev1.EventTypeWarning, reasonMissingHandle,
			"PV has no spec.csi.volumeHandle; releasing finalizer without deleting backing volume")
		return c.removeFinalizer(ctx, pv)
	}

	secrets, err := c.deletionSecrets(ctx, pv)
	if err != nil {
		return err
	}

	klog.InfoS("Deleting backing volume for terminating Retain PV",
		"pv", pv.Name, "volumeID", volumeID)
	c.eventf(pv, corev1.EventTypeNormal, reasonReclaiming,
		"Deleting backing volume %q via CSI DeleteVolume", volumeID)

	if err := c.deleter.DeleteVolume(ctx, volumeID, secrets); err != nil {
		c.eventf(pv, corev1.EventTypeWarning, reasonReclaimFailed,
			"DeleteVolume for %q failed: %v", volumeID, err)
		return fmt.Errorf("DeleteVolume %q: %w", volumeID, err)
	}

	klog.InfoS("Backing volume deleted, releasing finalizer", "pv", pv.Name, "volumeID", volumeID)
	c.eventf(pv, corev1.EventTypeNormal, reasonReclaimed,
		"Backing volume %q deleted", volumeID)
	return c.removeFinalizer(ctx, pv)
}

// deletionSecrets resolves the CSI DeleteVolume credentials from the annotations
// that external-provisioner recorded on the PV. A missing secret is treated the
// same way external-provisioner treats it: log and proceed without credentials,
// so a deleted secret namespace cannot wedge the PV forever. Transient API
// errors are returned so the item is retried.
func (c *Controller) deletionSecrets(ctx context.Context, pv *corev1.PersistentVolume) (map[string]string, error) {
	name := pv.Annotations[annDeletionSecretName]
	namespace := pv.Annotations[annDeletionSecretNamespace]
	if name == "" || namespace == "" {
		return nil, nil
	}

	secret, err := c.kubeClient.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.InfoS("Deletion secret not found, proceeding without credentials",
			"pv", pv.Name, "secret", namespace+"/"+name)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deletion secret %s/%s: %w", namespace, name, err)
	}

	credentials := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		credentials[k] = string(v)
	}
	return credentials, nil
}

func (c *Controller) addFinalizer(ctx context.Context, pv *corev1.PersistentVolume) error {
	updated := pv.DeepCopy()
	updated.Finalizers = append(updated.Finalizers, c.finalizer)
	if _, err := c.kubeClient.CoreV1().PersistentVolumes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("add finalizer to PV %s: %w", pv.Name, err)
	}
	klog.V(4).InfoS("Added reclaim finalizer", "pv", pv.Name)
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, pv *corev1.PersistentVolume) error {
	updated := pv.DeepCopy()
	updated.Finalizers = slices.DeleteFunc(updated.Finalizers, func(f string) bool {
		return f == c.finalizer
	})
	if _, err := c.kubeClient.CoreV1().PersistentVolumes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("remove finalizer from PV %s: %w", pv.Name, err)
	}
	klog.V(4).InfoS("Removed reclaim finalizer", "pv", pv.Name)
	return nil
}

func (c *Controller) eventf(pv *corev1.PersistentVolume, eventType, reason, format string, args ...interface{}) {
	if c.recorder == nil {
		return
	}
	c.recorder.Eventf(pv, eventType, reason, format, args...)
}
