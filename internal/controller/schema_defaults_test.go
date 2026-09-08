package controller

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// The default must live directly under the required storage object; placing it
// inside an optional retention object leaves an omitted retention at zero.
func TestMirrorRetentionDefaultIsReachable(t *testing.T) {
	raw, err := os.ReadFile("../../charts/falcon/crds/mirrors.zjusct.io_mirrors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema map[string]interface{} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	node := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, field := range []string{"spec", "storage", "retention"} {
		properties, ok := node["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("missing properties before %s", field)
		}
		node, ok = properties[field].(map[string]interface{})
		if !ok {
			t.Fatalf("missing schema for %s", field)
		}
	}
	if node["type"] != "integer" || node["default"] != float64(1) || node["minimum"] != float64(1) || node["maximum"] != float64(10) {
		t.Fatalf("retention must be a scalar defaulting to one historical generation: %#v", node)
	}
}
