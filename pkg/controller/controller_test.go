package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testDriver         = "test.csi.example.com"
	testFinalizer      = "csi-reclaim-controller/test-csi-example-com"
	testSkipAnnotation = testDriver + "/skip-reclaim"
	otherDriver        = "other.csi.example.com"
)

// fakeDeleter records DeleteVolume calls and can be told to fail.
type fakeDeleter struct {
	calls   []string
	secrets map[string]string
	err     error
}

func (f *fakeDeleter) DeleteVolume(_ context.Context, volumeID string, secrets map[string]string) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, volumeID)
	f.secrets = secrets
	return nil
}

type pvOption func(*corev1.PersistentVolume)

func withReclaimPolicy(p corev1.PersistentVolumeReclaimPolicy) pvOption {
	return func(pv *corev1.PersistentVolume) { pv.Spec.PersistentVolumeReclaimPolicy = p }
}

func withDriver(driver, handle string) pvOption {
	return func(pv *corev1.PersistentVolume) {
		pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: handle}
	}
}

func withNonCSISource() pvOption {
	return func(pv *corev1.PersistentVolume) {
		pv.Spec.CSI = nil
		pv.Spec.HostPath = &corev1.HostPathVolumeSource{Path: "/tmp/data"}
	}
}

func withFinalizer(f string) pvOption {
	return func(pv *corev1.PersistentVolume) { pv.Finalizers = append(pv.Finalizers, f) }
}

func withSkip() pvOption {
	return func(pv *corev1.PersistentVolume) {
		if pv.Annotations == nil {
			pv.Annotations = map[string]string{}
		}
		pv.Annotations[testSkipAnnotation] = "true"
	}
}

func withDeletionTimestamp() pvOption {
	return func(pv *corev1.PersistentVolume) {
		t := metav1.NewTime(time.Unix(1_700_000_000, 0))
		pv.DeletionTimestamp = &t
	}
}

func withPhase(phase corev1.PersistentVolumePhase) pvOption {
	return func(pv *corev1.PersistentVolume) { pv.Status.Phase = phase }
}

func withDeletionSecret(name, namespace string) pvOption {
	return func(pv *corev1.PersistentVolume) {
		if pv.Annotations == nil {
			pv.Annotations = map[string]string{}
		}
		pv.Annotations[annDeletionSecretName] = name
		pv.Annotations[annDeletionSecretNamespace] = namespace
	}
}

func newPV(name string, opts ...pvOption) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: "vol-" + name},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	for _, o := range opts {
		o(pv)
	}
	return pv
}

// newTestController wires a Controller against fake clients, pre-populating the
// informer cache with the given PVs (and optional secret objects).
func newTestController(t *testing.T, deleter VolumeDeleter, objects ...runtime.Object) (*Controller, *fake.Clientset) {
	t.Helper()

	client := fake.NewSimpleClientset(objects...)
	factory := informers.NewSharedInformerFactory(client, 0)
	pvInformer := factory.Core().V1().PersistentVolumes()

	for _, o := range objects {
		if pv, ok := o.(*corev1.PersistentVolume); ok {
			if err := pvInformer.Informer().GetStore().Add(pv); err != nil {
				t.Fatalf("seed informer: %v", err)
			}
		}
	}

	c, err := New(client, factory, deleter, testDriver, testFinalizer, testSkipAnnotation, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, client
}

func getPV(t *testing.T, client *fake.Clientset, name string) *corev1.PersistentVolume {
	t.Helper()
	pv, err := client.CoreV1().PersistentVolumes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get PV %s: %v", name, err)
	}
	return pv
}

func TestSyncAddsFinalizerToManagedPV(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1")
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}

	got := getPV(t, client, "pv1")
	if !slices.Contains(got.Finalizers, testFinalizer) {
		t.Errorf("expected finalizer %q to be added, finalizers=%v", testFinalizer, got.Finalizers)
	}
	if len(deleter.calls) != 0 {
		t.Errorf("did not expect DeleteVolume calls, got %v", deleter.calls)
	}
}

func TestSyncIgnoresDeletePolicyPV(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1", withReclaimPolicy(corev1.PersistentVolumeReclaimDelete))
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}

	got := getPV(t, client, "pv1")
	if slices.Contains(got.Finalizers, testFinalizer) {
		t.Errorf("did not expect finalizer on Delete-policy PV")
	}
}

func TestSyncIgnoresOtherDriverAndNonCSI(t *testing.T) {
	deleter := &fakeDeleter{}
	other := newPV("other", withDriver(otherDriver, "vol-other"))
	hostpath := newPV("hostpath", withNonCSISource())
	c, client := newTestController(t, deleter, other, hostpath)

	for _, name := range []string{"other", "hostpath"} {
		if err := c.syncPV(context.Background(), name); err != nil {
			t.Fatalf("syncPV %s: %v", name, err)
		}
		if slices.Contains(getPV(t, client, name).Finalizers, testFinalizer) {
			t.Errorf("did not expect finalizer on unmanaged PV %s", name)
		}
	}
}

func TestSyncReclaimsTerminatingPV(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1",
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}

	if len(deleter.calls) != 1 || deleter.calls[0] != "vol-pv1" {
		t.Fatalf("expected DeleteVolume(vol-pv1), got %v", deleter.calls)
	}
	got := getPV(t, client, "pv1")
	if slices.Contains(got.Finalizers, testFinalizer) {
		t.Errorf("expected finalizer to be removed after reclaim, finalizers=%v", got.Finalizers)
	}
}

func TestSyncDefersReclaimWhileBound(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1",
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeBound),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}

	if len(deleter.calls) != 0 {
		t.Errorf("expected no DeleteVolume while Bound, got %v", deleter.calls)
	}
	if !slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer retained while Bound")
	}
}

func TestSyncKeepsFinalizerWhenDeleteVolumeFails(t *testing.T) {
	deleter := &fakeDeleter{err: errors.New("backend unavailable")}
	pv := newPV("pv1",
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
	)
	c, client := newTestController(t, deleter, pv)

	err := c.syncPV(context.Background(), "pv1")
	if err == nil {
		t.Fatalf("expected error when DeleteVolume fails")
	}
	if !slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer retained after failed reclaim so it can be retried")
	}
}

func TestSyncPassesDeletionSecrets(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1",
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
		withDeletionSecret("csi-secret", "kube-system"),
	)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-secret", Namespace: "kube-system"},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("s3cr3t")},
	}
	c, _ := newTestController(t, deleter, pv, secret)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if deleter.secrets["username"] != "admin" || deleter.secrets["password"] != "s3cr3t" {
		t.Errorf("expected deletion secret passed to DeleteVolume, got %v", deleter.secrets)
	}
}

func TestSyncProceedsWhenDeletionSecretMissing(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1",
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
		withDeletionSecret("gone", "kube-system"),
	)
	c, client := newTestController(t, deleter, pv) // secret intentionally absent

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if len(deleter.calls) != 1 {
		t.Fatalf("expected DeleteVolume to proceed without credentials, got %v", deleter.calls)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer removed after reclaim")
	}
}

func TestSyncReleasesFinalizerWhenNoLongerManaged(t *testing.T) {
	deleter := &fakeDeleter{}
	// Policy changed to Delete after our finalizer was placed, PV now terminating.
	pv := newPV("pv1",
		withReclaimPolicy(corev1.PersistentVolumeReclaimDelete),
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if len(deleter.calls) != 0 {
		t.Errorf("must not delete storage for an unmanaged PV, got %v", deleter.calls)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer released for unmanaged terminating PV")
	}
}

func TestSyncCleansStaleFinalizerOnLivePV(t *testing.T) {
	deleter := &fakeDeleter{}
	// A live (not terminating) PV whose policy is now Delete but still carries
	// our finalizer should have it cleaned up.
	pv := newPV("pv1",
		withReclaimPolicy(corev1.PersistentVolumeReclaimDelete),
		withFinalizer(testFinalizer),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected stale finalizer removed from live unmanaged PV")
	}
}

func TestSyncReclaimsWithMissingVolumeHandle(t *testing.T) {
	deleter := &fakeDeleter{}
	pv := newPV("pv1",
		withDriver(testDriver, ""), // empty volume handle
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if len(deleter.calls) != 0 {
		t.Errorf("must not call DeleteVolume with empty handle, got %v", deleter.calls)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer released so PV is not stuck")
	}
}

func TestSyncSkipAnnotationPreventsReclaim(t *testing.T) {
	deleter := &fakeDeleter{}
	// Simulates linstor-affinity-controller recreating a Retain PV: the PV is
	// deleted with the skip annotation set. Its backing volume must survive and
	// our finalizer must be released so deletion can complete.
	pv := newPV("pv1",
		withSkip(),
		withFinalizer(testFinalizer),
		withDeletionTimestamp(),
		withPhase(corev1.VolumeReleased),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if len(deleter.calls) != 0 {
		t.Fatalf("skip annotation must prevent DeleteVolume, got %v", deleter.calls)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer released so the recreate can complete")
	}
}

func TestSyncSkipAnnotationDropsFinalizerOnLivePV(t *testing.T) {
	deleter := &fakeDeleter{}
	// A live PV that gets the skip annotation should be released from management
	// (finalizer removed) so it is out of the way of the recreate.
	pv := newPV("pv1",
		withSkip(),
		withFinalizer(testFinalizer),
	)
	c, client := newTestController(t, deleter, pv)

	if err := c.syncPV(context.Background(), "pv1"); err != nil {
		t.Fatalf("syncPV: %v", err)
	}
	if slices.Contains(getPV(t, client, "pv1").Finalizers, testFinalizer) {
		t.Errorf("expected finalizer removed while skip annotation is set")
	}
}
