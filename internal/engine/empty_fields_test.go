package engine

import (
	"reflect"
	"testing"
)

// Upstream's KubeVirt CR sets empty structs and an empty gate list. Applied as
// they are, they show as {} leaves in k4all-operator's managedFields, which
// consoles read as k4all owning everything below (spec R5, §16.3).
func TestStripEmptyFields_KubeVirt(t *testing.T) {
	obj := decodeOne(t, upstreamKubeVirt)
	stripEmptyFields(obj)
	want := map[string]interface{}{
		"configuration":   map[string]interface{}{"imagePullPolicy": "IfNotPresent"},
		"imagePullPolicy": "IfNotPresent",
	}
	if got := obj.Object["spec"]; !reflect.DeepEqual(got, want) {
		t.Errorf("spec = %v, want %v", got, want)
	}
}

// CDI's CR has no empty field today; whatever it sets stays.
func TestStripEmptyFields_CDIKeepsWhatItSets(t *testing.T) {
	obj := decodeOne(t, upstreamCDI)
	want := obj.DeepCopy()
	stripEmptyFields(obj)
	if !reflect.DeepEqual(obj.Object, want.Object) {
		t.Errorf("CDI = %v, want it unchanged %v", obj.Object, want.Object)
	}

	empty := decodeOne(t, upstreamCDI)
	empty.Object["spec"].(map[string]interface{})["workload"] = map[string]interface{}{}
	stripEmptyFields(empty)
	if _, ok := empty.Object["spec"].(map[string]interface{})["workload"]; ok {
		t.Error("CDI spec.workload: {} kept, want it left out")
	}
}

// Other kinds are applied as they come: an empty map or list can mean
// something there (an emptyDir: {} volume).
func TestStripEmptyFields_OtherKindsUntouched(t *testing.T) {
	obj := decodeOne(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: x
data: {}
`)
	stripEmptyFields(obj)
	if _, ok := obj.Object["data"]; !ok {
		t.Error("ConfigMap data: {} removed, want other kinds untouched")
	}
}
