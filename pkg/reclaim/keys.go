// Package reclaim defines the annotation and finalizer keys that
// csi-reclaim-controller uses, so other projects can interoperate with it
// without hardcoding the strings. The keys are qualified per CSI driver, so
// they are unique per driver.
//
// The package intentionally has no Kubernetes or third-party dependencies, to
// keep it cheap to import.
package reclaim

import (
	"regexp"
	"strconv"
)

const (
	// SkipAnnotationName is the name segment of the annotation that excludes a
	// PersistentVolume from reclaiming. The full key is
	// "<csi-driver-name>/skip-reclaim" (see SkipAnnotation). Set it to a true
	// value to keep the backing volume when the PV is deleted — for example
	// while deleting and recreating a PV in place to change an immutable field.
	SkipAnnotationName = "skip-reclaim"

	// FinalizerPrefix is the prefix segment of the finalizer
	// csi-reclaim-controller places on the PVs it manages. The full key is
	// "csi-reclaim-controller/<sanitized-driver-name>" (see Finalizer),
	// mirroring the way external-attacher names its finalizers.
	FinalizerPrefix = "csi-reclaim-controller"
)

// driverNameSanitizer matches every character that is not allowed in the name
// segment of a finalizer, so it can be replaced with a dash. This matches how
// external-attacher sanitizes driver names.
var driverNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9-]`)

// SkipAnnotation returns the annotation key for the given CSI driver that, when
// set to a true value on a PV, excludes it from reclaiming.
func SkipAnnotation(driverName string) string {
	return driverName + "/" + SkipAnnotationName
}

// Finalizer returns the finalizer key that csi-reclaim-controller places on
// managed PVs of the given CSI driver, e.g. "linstor.csi.linbit.com" yields
// "csi-reclaim-controller/linstor-csi-linbit-com".
func Finalizer(driverName string) string {
	return FinalizerPrefix + "/" + driverNameSanitizer.ReplaceAllString(driverName, "-")
}

// ShouldSkip reports whether the given PV annotations opt the volume out of
// reclaiming for driverName, i.e. the skip annotation is present and set to a
// value that parses as true (per strconv.ParseBool: "true", "1", "t", …).
func ShouldSkip(annotations map[string]string, driverName string) bool {
	skip, err := strconv.ParseBool(annotations[SkipAnnotation(driverName)])
	return err == nil && skip
}
