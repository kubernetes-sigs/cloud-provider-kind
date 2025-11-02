package gateway

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
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
	logger := klog.FromContext(ctx)

	crdGVR := schema.GroupVersionResource{
		Group:    crdGroup,
		Version:  crdVersion,
		Resource: crdResource,
	}

	targetDir := filepath.Join(crdsDir, string(channelDir))

	logger.Info("Walking embedded directory for channel", "channel", channelDir, "targetDir", targetDir)
	err := fs.WalkDir(crdFS, targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("error walking embedded fs at %q: %w", path, err)
		}

		// Skip directories
		if d.IsDir() {
			// Skip the root directory itself, only process files within it
			if path == targetDir {
				logger.V(4).Info("Entering directory", "path", path)
				return nil
			}
			logger.V(4).Info("Skipping directory", "path", path)
			return nil // Continue walking
		}

		// Process only YAML files
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			logger.V(4).Info("Skipping non-yaml file", "path", path)
			return nil
		}

		logger.Info("Processing embedded CRD file", "path", path)
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
				logger.Info(
					"Skipping object in with unexpected kind/apiVersion",
					"path", path,
					"object", klog.KObj(obj))
				continue
			}

			crdName := obj.GetName()
			logger.Info("Attempting to create CRD", "crdName", crdName)
			_, createErr := m.dynamicClient.Resource(crdGVR).Create(ctx, obj, metav1.CreateOptions{})
			if createErr != nil {
				if errors.IsAlreadyExists(createErr) {
					logger.Info("CRD already exists, skipping creation", "crdName", crdName)
					// TODO: Consider updating/patching if needed, but for CRDs, create-if-not-exists is often sufficient.
				} else {
					return fmt.Errorf("failed to create CRD %q from file %q: %w", crdName, path, createErr)
				}
			} else {
				logger.Info("Successfully created CRD", "crdName", crdName)
			}
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("error processing embedded CRDs from %s: %w", targetDir, err)
	}

	logger.Info("Finished processing embedded CRDs.")
	return nil
}
