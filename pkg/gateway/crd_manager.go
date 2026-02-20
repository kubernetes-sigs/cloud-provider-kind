package gateway

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-kind/pkg/config"
)

//go:embed crds/standard/*.yaml crds/experimental/*.yaml
var crdFS embed.FS

const (
	crdsDir = "crds" // Base directory within the embedded FS
	// use constants to avoid the dependency on apiextensions
	crdKind       = "CustomResourceDefinition"
	crdResource   = "customresourcedefinitions"
	crdGroup      = "apiextensions.k8s.io"
	crdVersion    = "v1"
	crdAPIVersion = "apiextensions.k8s.io/v1"
)

// CRDManager handles the installation of Gateway API CRDs.
type CRDManager struct {
	dynamicClient dynamic.Interface
}

// NewCRDManager creates a new CRDManager instance.
// It attempts to load in-cluster config first, then falls back to kubeconfig.
func NewCRDManager(config *rest.Config) (*CRDManager, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return &CRDManager{
		dynamicClient: dynamicClient,
	}, nil
}

// InstallCRDs reads CRDs from the embedded filesystem and applies them to the cluster.
func (m *CRDManager) InstallCRDs(ctx context.Context, channelDir config.GatewayReleaseChannel) error {
	crdGVR := schema.GroupVersionResource{
		Group:    crdGroup,
		Version:  crdVersion,
		Resource: crdResource,
	}

	// embed.FS always uses Unix path names
	targetDir := path.Join(crdsDir, string(channelDir))

	klog.Infof("Walking embedded directory for channel %q: %s", channelDir, targetDir)
	err := fs.WalkDir(crdFS, targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error walking embedded fs at %q: %w", path, err)
		}

		// Skip directories
		if d.IsDir() {
			// Skip the root directory itself, only process files within it
			if path == targetDir {
				klog.V(4).Infof("Entering directory: %s", path)
				return nil
			}
			klog.V(4).Infof("Skipping directory: %s", path)
			return nil // Continue walking
		}

		// Skip the kustomize files
		if strings.HasSuffix(path, "kustomization.yaml") {
			return nil
		}

		// Process only YAML files
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			klog.V(4).Infof("Skipping non-yaml file: %s", path)
			return nil
		}

		klog.Infof("Processing embedded CRD file: %s", path)
		file, err := crdFS.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open embedded file %q: %w", path, err)
		}
		defer file.Close()

		// Use a YAML decoder to handle multi-document files
		decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if err == io.EOF {
					break // End of file
				}
				return fmt.Errorf("failed to decode YAML document from %q: %w", path, err)
			}

			// Basic validation
			if obj.GetKind() != crdKind || obj.GetAPIVersion() != crdAPIVersion {
				klog.Warningf("Skipping object in %q with unexpected kind/apiVersion: %s/%s", path, obj.GetAPIVersion(), obj.GetKind())
				continue
			}

			crdName := obj.GetName()
			klog.Infof("Attempting to create CRD: %s", crdName)
			_, createErr := m.dynamicClient.Resource(crdGVR).Create(ctx, obj, metav1.CreateOptions{})
			if createErr != nil {
				if errors.IsAlreadyExists(createErr) {
					klog.Infof("CRD %q already exists, skipping creation.", crdName)
					// TODO: Consider updating/patching if needed, but for CRDs, create-if-not-exists is often sufficient.
				} else {
					return fmt.Errorf("failed to create CRD %q from file %q: %w", crdName, path, createErr)
				}
			} else {
				klog.Infof("Successfully created CRD: %s", crdName)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error processing embedded CRDs from %s: %w", targetDir, err)
	}

	klog.Info("Finished processing embedded CRDs.")
	return nil
}
