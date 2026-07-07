// Package reclaim defines the annotation and finalizer keys that
// csi-reclaim-controller uses, so other projects can interoperate with it
// without hardcoding the strings. All keys are namespaced by the CSI driver
// name, so they are unique per driver.
//
// The package intentionally has no Kubernetes or third-party dependencies, to
// keep it cheap to import.
package reclaim

import "strconv"

const (
	// SkipAnnotationName is the name segment of the annotation that excludes a
	// PersistentVolume from reclaiming. The full key is
	// "<csi-driver-name>/skip-reclaim" (see SkipAnnotation). Set it to a true
	// value to keep the backing volume when the PV is deleted — for example
	// while deleting and recreating a PV in place to change an immutable field.
	SkipAnnotationName = "skip-reclaim"

	// FinalizerName is the name segment of the finalizer csi-reclaim-controller
	// places on the PVs it manages. The full key is
	// "<csi-driver-name>/reclaim-protection" (see Finalizer).
	FinalizerName = "reclaim-protection"
)

// SkipAnnotation returns the annotation key for the given CSI driver that, when
// set to a true value on a PV, excludes it from reclaiming.
func SkipAnnotation(driverName string) string {
	return driverName + "/" + SkipAnnotationName
}

// Finalizer returns the finalizer key that csi-reclaim-controller places on
// managed PVs of the given CSI driver.
func Finalizer(driverName string) string {
	return driverName + "/" + FinalizerName
}

// ShouldSkip reports whether the given PV annotations opt the volume out of
// reclaiming for driverName, i.e. the skip annotation is present and set to a
// value that parses as true (per strconv.ParseBool: "true", "1", "t", …).
func ShouldSkip(annotations map[string]string, driverName string) bool {
	skip, err := strconv.ParseBool(annotations[SkipAnnotation(driverName)])
	return err == nil && skip
}
