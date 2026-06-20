package controller

import (
	"testing"

	"k8s.io/client-go/rest"
)

func Test_selectRestConfig(t *testing.T) {
	internal := &rest.Config{Host: "https://internal:6443"}
	external := &rest.Config{Host: "https://external:6443"}

	// internal kubeconfig unavailable, only the external endpoint answered the
	// probe: must not panic and must return the external config.
	got, err := selectRestConfig(nil, external, external.Host)
	if err != nil {
		t.Fatalf("selectRestConfig() error = %v", err)
	}
	if got != external {
		t.Errorf("selectRestConfig() = %v, want external config", got)
	}

	// external kubeconfig unavailable, only the internal endpoint answered.
	got, err = selectRestConfig(internal, nil, internal.Host)
	if err != nil {
		t.Fatalf("selectRestConfig() error = %v", err)
	}
	if got != internal {
		t.Errorf("selectRestConfig() = %v, want internal config", got)
	}

	// host does not match any available config.
	if _, err := selectRestConfig(nil, external, ""); err == nil {
		t.Errorf("selectRestConfig() expected error for unmatched host, got nil")
	}
}
