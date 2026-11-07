package v1alpha1

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestSmbQuarantineStatusCarriesNoNames guards the privacy rule: the status
// of an SmbQuarantine may only hold counts, sizes, times, a truncation flag
// and conditions -- never a field that could carry a file or directory name
// from the quarantined data.
func TestSmbQuarantineStatusCarriesNoNames(t *testing.T) {
	allowed := map[reflect.Type]bool{
		reflect.TypeFor[int64]():              true,
		reflect.TypeFor[bool]():               true,
		reflect.TypeFor[*metav1.Time]():       true,
		reflect.TypeFor[[]metav1.Condition](): true,
	}
	st := reflect.TypeFor[SmbQuarantineStatus]()
	for i := range st.NumField() {
		f := st.Field(i)
		if !allowed[f.Type] {
			t.Errorf("SmbQuarantineStatus.%s has type %s; only int64, bool, *metav1.Time and []metav1.Condition are allowed", f.Name, f.Type)
		}
	}
}
