package reclaim

import "testing"

const driver = "linstor.csi.linbit.com"

func TestSkipAnnotation(t *testing.T) {
	if got, want := SkipAnnotation(driver), "linstor.csi.linbit.com/skip-reclaim"; got != want {
		t.Errorf("SkipAnnotation(%q) = %q, want %q", driver, got, want)
	}
}

func TestFinalizer(t *testing.T) {
	if got, want := Finalizer(driver), "csi-reclaim-controller/linstor-csi-linbit-com"; got != want {
		t.Errorf("Finalizer(%q) = %q, want %q", driver, got, want)
	}
}

func TestShouldSkip(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "absent", set: false, want: false},
		{name: "true", value: "true", set: true, want: true},
		{name: "one", value: "1", set: true, want: true},
		{name: "false", value: "false", set: true, want: false},
		{name: "empty", value: "", set: true, want: false},
		{name: "garbage", value: "yes-please", set: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{}
			if tc.set {
				annotations[SkipAnnotation(driver)] = tc.value
			}
			if got := ShouldSkip(annotations, driver); got != tc.want {
				t.Errorf("ShouldSkip(%v) = %v, want %v", annotations, got, tc.want)
			}
		})
	}
}
