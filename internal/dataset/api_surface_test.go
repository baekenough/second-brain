package dataset

import (
	"reflect"
	"testing"
)

// TestHoldoutSet_ExportedSurfaceIsFrozen is the guard that keeps validation-set
// isolation structural rather than customary. HoldoutSet must never grow an
// exported way to hand its pairs, queries or document IDs to a caller: an
// optimiser that can read the holdout set will, sooner or later, be tuned
// against it, and then the holdout number stops meaning anything.
//
// If this test fails, do not update `want`. Remove the accessor.
func TestHoldoutSet_ExportedSurfaceIsFrozen(t *testing.T) {
	t.Parallel()
	got := exportedMethodNames(reflect.TypeOf(&HoldoutSet{}))
	want := []string{"Evaluate", "Queries"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HoldoutSet exported methods = %v, want %v — the public surface of "+
			"HoldoutSet changed, so validation-set isolation may be broken", got, want)
	}
}

// TestHoldoutSet_HasNoExportedFields complements the method check: an exported
// field would leak the pairs just as effectively as an accessor.
func TestHoldoutSet_HasNoExportedFields(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(HoldoutSet{})
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.IsExported() {
			t.Errorf("HoldoutSet has exported field %q — it leaks holdout data", f.Name)
		}
	}
}

func exportedMethodNames(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		if m := t.Method(i); m.IsExported() {
			names = append(names, m.Name)
		}
	}
	return names
}
