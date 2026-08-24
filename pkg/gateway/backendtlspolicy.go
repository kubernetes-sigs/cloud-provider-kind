/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gateway

import (
	"fmt"
	"sort"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (c *Controller) lookupBackendTLSPolicy(serviceNamespace, serviceName string) *gatewayv1.BackendTLSPolicy {
	policies, err := c.backendTLSPolicyLister.BackendTLSPolicies(serviceNamespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list BackendTLSPolicies in namespace %s: %v", serviceNamespace, err)
		return nil
	}

	var matching []*gatewayv1.BackendTLSPolicy
	for _, policy := range policies {
		for _, targetRef := range policy.Spec.TargetRefs {
			if targetRef.Group != "" || targetRef.Kind != "Service" {
				continue
			}
			if string(targetRef.Name) == serviceName {
				matching = append(matching, policy)
				break
			}
		}
	}

	if len(matching) == 0 {
		return nil
	}

	sort.Slice(matching, func(i, j int) bool {
		if !matching[i].CreationTimestamp.Equal(&matching[j].CreationTimestamp) {
			return matching[i].CreationTimestamp.Before(&matching[j].CreationTimestamp)
		}
		keyI := matching[i].Namespace + "/" + matching[i].Name
		keyJ := matching[j].Namespace + "/" + matching[j].Name
		return keyI < keyJ
	})

	return matching[0]
}

func (c *Controller) buildUpstreamTLSContext(policy *gatewayv1.BackendTLSPolicy) (*anypb.Any, error) {
	if len(policy.Spec.Validation.CACertificateRefs) == 0 {
		return nil, fmt.Errorf("no CACertificateRefs in BackendTLSPolicy %s/%s", policy.Namespace, policy.Name)
	}

	caRef := policy.Spec.Validation.CACertificateRefs[0]

	if string(caRef.Group) != "" {
		return nil, fmt.Errorf("unsupported CACertificateRef group %q in BackendTLSPolicy %s/%s", caRef.Group, policy.Namespace, policy.Name)
	}
	if string(caRef.Kind) != "ConfigMap" {
		return nil, fmt.Errorf("unsupported CACertificateRef kind %q in BackendTLSPolicy %s/%s (only ConfigMap is supported)", caRef.Kind, policy.Namespace, policy.Name)
	}

	cm, err := c.configMapLister.ConfigMaps(policy.Namespace).Get(string(caRef.Name))
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap %s/%s: %w", policy.Namespace, caRef.Name, err)
	}

	var trustedCA *corev3.DataSource
	if caCert, ok := cm.Data["ca.crt"]; ok {
		trustedCA = &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineString{
				InlineString: caCert,
			},
		}
	} else if caCertBytes, ok := cm.BinaryData["ca.crt"]; ok {
		trustedCA = &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineBytes{
				InlineBytes: caCertBytes,
			},
		}
	} else {
		return nil, fmt.Errorf("ConfigMap %s/%s does not contain key 'ca.crt'", policy.Namespace, caRef.Name)
	}

	upstreamTLS := &tlsv3.UpstreamTlsContext{
		Sni: string(policy.Spec.Validation.Hostname),
		CommonTlsContext: &tlsv3.CommonTlsContext{
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
				ValidationContext: &tlsv3.CertificateValidationContext{
					TrustedCa: trustedCA,
				},
			},
		},
	}

	return anypb.New(upstreamTLS)
}
