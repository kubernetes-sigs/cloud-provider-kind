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
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const maxPolicyAncestors = 16

// backendTLSError is returned when a BackendTLSPolicy cannot be applied.
// The dataplane must fail the client with HTTP 5xx and must not use plaintext.
type backendTLSError struct {
	reason  gatewayv1.PolicyConditionReason
	message string
}

func (e *backendTLSError) Error() string {
	return e.message
}

func isBackendTLSError(err error) bool {
	_, ok := err.(*backendTLSError)
	return ok
}

func sortBackendTLSPolicies(policies []*gatewayv1.BackendTLSPolicy) {
	sort.Slice(policies, func(i, j int) bool {
		if !policies[i].CreationTimestamp.Equal(&policies[j].CreationTimestamp) {
			return policies[i].CreationTimestamp.Before(&policies[j].CreationTimestamp)
		}
		keyI := policies[i].Namespace + "/" + policies[i].Name
		keyJ := policies[j].Namespace + "/" + policies[j].Name
		return keyI < keyJ
	})
}

func isServiceTargetRef(ref gatewayv1.LocalPolicyTargetReferenceWithSectionName) bool {
	if ref.Group != "" && ref.Group != "core" {
		return false
	}
	return ref.Kind == "Service"
}

func targetRefSectionName(ref gatewayv1.LocalPolicyTargetReferenceWithSectionName) string {
	if ref.SectionName == nil {
		return ""
	}
	return string(*ref.SectionName)
}

func policyConflictKey(namespace string, ref gatewayv1.LocalPolicyTargetReferenceWithSectionName) string {
	return namespace + "/" + string(ref.Group) + "/" + string(ref.Kind) + "/" + string(ref.Name) + "/" + targetRefSectionName(ref)
}

// lookupBackendTLSPolicy returns the BackendTLSPolicy that applies to the
// Service and port. A policy with a matching sectionName (Service port name)
// takes precedence over a policy that targets the whole Service.
// On ties, the oldest policy wins, then {namespace}/{name} in alphabetical order.
func (c *Controller) lookupBackendTLSPolicy(serviceNamespace, serviceName, portName string) *gatewayv1.BackendTLSPolicy {
	policies, err := c.backendTLSPolicyLister.BackendTLSPolicies(serviceNamespace).List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list BackendTLSPolicies in namespace %s: %v", serviceNamespace, err)
		return nil
	}

	var specific, whole []*gatewayv1.BackendTLSPolicy
	for _, policy := range policies {
		matches, isSpecific := policyMatchesPort(policy, serviceName, portName)
		if !matches {
			continue
		}
		if isSpecific {
			specific = append(specific, policy)
		} else {
			whole = append(whole, policy)
		}
	}

	candidates := whole
	if len(specific) > 0 {
		candidates = specific
	}
	if len(candidates) == 0 {
		return nil
	}
	sortBackendTLSPolicies(candidates)
	return candidates[0]
}

// policyMatchesPort reports whether the policy targets the Service and whether
// that match is sectionName-specific for portName.
func policyMatchesPort(policy *gatewayv1.BackendTLSPolicy, serviceName, portName string) (matches bool, specific bool) {
	for _, ref := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(ref) || string(ref.Name) != serviceName {
			continue
		}
		section := targetRefSectionName(ref)
		if section == "" {
			matches = true
			continue
		}
		if portName != "" && section == portName {
			return true, true
		}
	}
	return matches, false
}

func servicePortName(service *corev1.Service, backendRef gatewayv1.BackendRef) string {
	if backendRef.Port == nil {
		return ""
	}
	want := int32(*backendRef.Port)
	for _, p := range service.Spec.Ports {
		if p.Port == want {
			return p.Name
		}
	}
	return ""
}

func (c *Controller) resolveCACertificate(policy *gatewayv1.BackendTLSPolicy) (*corev3.DataSource, error) {
	if policy.Spec.Validation.WellKnownCACertificates != nil && *policy.Spec.Validation.WellKnownCACertificates != "" {
		return nil, &backendTLSError{
			reason:  gatewayv1.PolicyReasonInvalid,
			message: fmt.Sprintf("WellKnownCACertificates is not supported in BackendTLSPolicy %s/%s", policy.Namespace, policy.Name),
		}
	}

	refs := policy.Spec.Validation.CACertificateRefs
	if len(refs) == 0 {
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonNoValidCACertificate,
			message: fmt.Sprintf("no CACertificateRefs in BackendTLSPolicy %s/%s", policy.Namespace, policy.Name),
		}
	}

	var lastErr error
	validCount := 0
	var trustedCA *corev3.DataSource
	for _, ref := range refs {
		ds, err := c.loadCACertificateRef(policy, ref)
		if err != nil {
			lastErr = err
			continue
		}
		validCount++
		if trustedCA == nil {
			trustedCA = ds
		}
	}
	if validCount == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonNoValidCACertificate,
			message: fmt.Sprintf("no valid CACertificateRefs in BackendTLSPolicy %s/%s", policy.Namespace, policy.Name),
		}
	}
	if lastErr != nil {
		// At least one ref is invalid. Fail the connection (no plaintext fallback).
		return nil, lastErr
	}
	return trustedCA, nil
}

func (c *Controller) loadCACertificateRef(policy *gatewayv1.BackendTLSPolicy, ref gatewayv1.LocalObjectReference) (*corev3.DataSource, error) {
	if string(ref.Group) != "" && string(ref.Group) != "core" {
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonInvalidKind,
			message: fmt.Sprintf("unsupported CACertificateRef group %q in BackendTLSPolicy %s/%s", ref.Group, policy.Namespace, policy.Name),
		}
	}
	if string(ref.Kind) != "ConfigMap" {
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonInvalidKind,
			message: fmt.Sprintf("unsupported CACertificateRef kind %q in BackendTLSPolicy %s/%s (only ConfigMap is supported)", ref.Kind, policy.Namespace, policy.Name),
		}
	}

	cm, err := c.configMapLister.ConfigMaps(policy.Namespace).Get(string(ref.Name))
	if err != nil {
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef,
			message: fmt.Sprintf("failed to get ConfigMap %s/%s: %v", policy.Namespace, ref.Name, err),
		}
	}

	trustedCA, err := caCertFromConfigMap(cm)
	if err != nil {
		return nil, &backendTLSError{
			reason:  gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef,
			message: fmt.Sprintf("ConfigMap %s/%s: %v", policy.Namespace, ref.Name, err),
		}
	}
	return trustedCA, nil
}

func caCertFromConfigMap(cm *corev1.ConfigMap) (*corev3.DataSource, error) {
	if caCert, ok := cm.Data["ca.crt"]; ok {
		if strings.TrimSpace(caCert) == "" {
			return nil, fmt.Errorf("key 'ca.crt' is empty")
		}
		return &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineString{
				InlineString: caCert,
			},
		}, nil
	}
	if caCertBytes, ok := cm.BinaryData["ca.crt"]; ok {
		if len(bytes.TrimSpace(caCertBytes)) == 0 {
			return nil, fmt.Errorf("key 'ca.crt' is empty")
		}
		return &corev3.DataSource{
			Specifier: &corev3.DataSource_InlineBytes{
				InlineBytes: caCertBytes,
			},
		}, nil
	}
	return nil, fmt.Errorf("does not contain key 'ca.crt'")
}

func (c *Controller) buildUpstreamTLSContext(policy *gatewayv1.BackendTLSPolicy) (*anypb.Any, error) {
	trustedCA, err := c.resolveCACertificate(policy)
	if err != nil {
		return nil, err
	}

	hostname := string(policy.Spec.Validation.Hostname)
	upstreamTLS := &tlsv3.UpstreamTlsContext{
		Sni: hostname,
		CommonTlsContext: &tlsv3.CommonTlsContext{
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
				ValidationContext: &tlsv3.CertificateValidationContext{
					TrustedCa: trustedCA,
					// Hostname is SNI and the certificate identity. Envoy does not
					// authenticate the hostname from SNI plus TrustedCa alone.
					MatchTypedSubjectAltNames: []*tlsv3.SubjectAltNameMatcher{{
						SanType: tlsv3.SubjectAltNameMatcher_DNS,
						Matcher: &matcherv3.StringMatcher{
							MatchPattern: &matcherv3.StringMatcher_Exact{
								Exact: hostname,
							},
						},
					}},
				},
			},
		},
	}

	return anypb.New(upstreamTLS)
}

// rewriteRoutesForInvalidBackendTLS replaces cluster routing with HTTP 503
// when the BackendTLSPolicy CA is invalid. The spec requires HTTP 5xx and
// forbids a plaintext fallback.
func rewriteRoutesForInvalidBackendTLS(routes []*routev3.Route, invalidClusters map[string]struct{}) {
	if len(invalidClusters) == 0 {
		return
	}
	for _, route := range routes {
		if routeUsesAnyCluster(route, invalidClusters) {
			route.Action = &routev3.Route_DirectResponse{
				DirectResponse: &routev3.DirectResponseAction{Status: 503},
			}
		}
	}
}

func routeUsesAnyCluster(route *routev3.Route, clusters map[string]struct{}) bool {
	action, ok := route.GetAction().(*routev3.Route_Route)
	if !ok || action.Route == nil {
		return false
	}
	switch spec := action.Route.ClusterSpecifier.(type) {
	case *routev3.RouteAction_Cluster:
		_, found := clusters[spec.Cluster]
		return found
	case *routev3.RouteAction_WeightedClusters:
		if spec.WeightedClusters == nil {
			return false
		}
		for _, w := range spec.WeightedClusters.Clusters {
			if _, found := clusters[w.Name]; found {
				return true
			}
		}
	}
	return false
}

func (c *Controller) conflictWinners() map[string]*gatewayv1.BackendTLSPolicy {
	winners := make(map[string]*gatewayv1.BackendTLSPolicy)
	policies, err := c.backendTLSPolicyLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list BackendTLSPolicies: %v", err)
		return winners
	}
	grouped := make(map[string][]*gatewayv1.BackendTLSPolicy)
	for _, policy := range policies {
		for _, ref := range policy.Spec.TargetRefs {
			if !isServiceTargetRef(ref) {
				continue
			}
			key := policyConflictKey(policy.Namespace, ref)
			grouped[key] = append(grouped[key], policy)
		}
	}
	for key, group := range grouped {
		sortBackendTLSPolicies(group)
		winners[key] = group[0]
	}
	return winners
}

func policyIsConflicted(policy *gatewayv1.BackendTLSPolicy, winners map[string]*gatewayv1.BackendTLSPolicy) bool {
	for _, ref := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(ref) {
			continue
		}
		winner := winners[policyConflictKey(policy.Namespace, ref)]
		if winner != nil && (winner.Namespace != policy.Namespace || winner.Name != policy.Name) {
			return true
		}
	}
	return false
}

func (c *Controller) servicesUsedByGateway(gw *gatewayv1.Gateway) map[string]struct{} {
	used := make(map[string]struct{})
	for _, route := range c.getHTTPRoutesForGateway(gw) {
		for _, rule := range route.Spec.Rules {
			for _, backendRef := range rule.BackendRefs {
				if backendRef.Kind != nil && *backendRef.Kind != "Service" {
					continue
				}
				if backendRef.Group != nil && *backendRef.Group != "" {
					continue
				}
				ns := route.Namespace
				if backendRef.Namespace != nil {
					ns = string(*backendRef.Namespace)
				}
				used[ns+"/"+string(backendRef.Name)] = struct{}{}
			}
		}
	}
	return used
}

func policyTargetsUsedService(policy *gatewayv1.BackendTLSPolicy, used map[string]struct{}) bool {
	for _, ref := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(ref) {
			continue
		}
		if _, ok := used[policy.Namespace+"/"+string(ref.Name)]; ok {
			return true
		}
	}
	return false
}

func (c *Controller) inspectTargetRefs(policy *gatewayv1.BackendTLSPolicy) *backendTLSError {
	if c.serviceLister == nil {
		return nil
	}
	for _, ref := range policy.Spec.TargetRefs {
		if !isServiceTargetRef(ref) {
			continue
		}
		svc, err := c.serviceLister.Services(policy.Namespace).Get(string(ref.Name))
		if err != nil {
			return &backendTLSError{
				reason:  gatewayv1.PolicyReasonTargetNotFound,
				message: fmt.Sprintf("Service %s/%s was not found", policy.Namespace, ref.Name),
			}
		}
		section := targetRefSectionName(ref)
		if section == "" {
			continue
		}
		found := false
		for _, p := range svc.Spec.Ports {
			if p.Name == section {
				found = true
				break
			}
		}
		if !found {
			return &backendTLSError{
				reason:  gatewayv1.PolicyReasonTargetNotFound,
				message: fmt.Sprintf("sectionName %q does not exist on Service %s/%s", section, policy.Namespace, ref.Name),
			}
		}
	}
	return nil
}

func backendTLSPolicyConditions(generation int64, conflicted bool, caErr *backendTLSError, targetErr *backendTLSError) []metav1.Condition {
	now := metav1.Now()
	accepted := metav1.Condition{
		Type:               string(gatewayv1.PolicyConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.PolicyReasonAccepted),
		Message:            "Policy is accepted.",
		ObservedGeneration: generation,
		LastTransitionTime: now,
	}
	resolved := metav1.Condition{
		Type:               string(gatewayv1.BackendTLSPolicyConditionResolvedRefs),
		Status:             metav1.ConditionTrue,
		Reason:             string(gatewayv1.BackendTLSPolicyReasonResolvedRefs),
		Message:            "All object references were resolved.",
		ObservedGeneration: generation,
		LastTransitionTime: now,
	}

	if caErr != nil {
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = string(caErr.reason)
		resolved.Message = caErr.message
		if caErr.reason == gatewayv1.BackendTLSPolicyReasonInvalidKind ||
			caErr.reason == gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef ||
			caErr.reason == gatewayv1.BackendTLSPolicyReasonNoValidCACertificate {
			accepted.Status = metav1.ConditionFalse
			accepted.Reason = string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate)
			accepted.Message = caErr.message
		}
		if caErr.reason == gatewayv1.PolicyReasonInvalid {
			accepted.Status = metav1.ConditionFalse
			accepted.Reason = string(gatewayv1.PolicyReasonInvalid)
			accepted.Message = caErr.message
		}
	}

	if targetErr != nil {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = string(targetErr.reason)
		accepted.Message = targetErr.message
		resolved.Status = metav1.ConditionFalse
		resolved.Reason = string(targetErr.reason)
		resolved.Message = targetErr.message
	}

	if conflicted {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = string(gatewayv1.PolicyReasonConflicted)
		accepted.Message = "Policy conflicts with another BackendTLSPolicy that targets the same Service and sectionName."
	}

	return []metav1.Condition{accepted, resolved}
}

func gatewayAncestorRef(gw *gatewayv1.Gateway) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{
		Group:     ptr.To(gatewayv1.Group(gatewayv1.GroupName)),
		Kind:      ptr.To(gatewayv1.Kind("Gateway")),
		Namespace: ptr.To(gatewayv1.Namespace(gw.Namespace)),
		Name:      gatewayv1.ObjectName(gw.Name),
	}
}

func parentRefKey(ref gatewayv1.ParentReference) string {
	ns := ""
	if ref.Namespace != nil {
		ns = string(*ref.Namespace)
	}
	kind := ""
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}
	return kind + "/" + ns + "/" + string(ref.Name)
}

func upsertPolicyAncestor(status *gatewayv1.PolicyStatus, ancestor gatewayv1.PolicyAncestorStatus) {
	for i := range status.Ancestors {
		existing := status.Ancestors[i]
		if existing.ControllerName == ancestor.ControllerName &&
			parentRefKey(existing.AncestorRef) == parentRefKey(ancestor.AncestorRef) {
			merged := existing.Conditions
			for _, cond := range ancestor.Conditions {
				meta.SetStatusCondition(&merged, cond)
			}
			ancestor.Conditions = merged
			status.Ancestors[i] = ancestor
			return
		}
	}
	if len(status.Ancestors) >= maxPolicyAncestors {
		return
	}
	status.Ancestors = append(status.Ancestors, ancestor)
}

func removePolicyAncestor(status *gatewayv1.PolicyStatus, controller gatewayv1.GatewayController, ref gatewayv1.ParentReference) {
	kept := make([]gatewayv1.PolicyAncestorStatus, 0, len(status.Ancestors))
	for _, ancestor := range status.Ancestors {
		if ancestor.ControllerName == controller && parentRefKey(ancestor.AncestorRef) == parentRefKey(ref) {
			continue
		}
		kept = append(kept, ancestor)
	}
	status.Ancestors = kept
}

func (c *Controller) updateBackendTLSPolicyStatuses(ctx context.Context, gw *gatewayv1.Gateway) error {
	if c.gwClient == nil || c.backendTLSPolicyLister == nil {
		return nil
	}

	policies, err := c.backendTLSPolicyLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list BackendTLSPolicies: %w", err)
	}

	used := c.servicesUsedByGateway(gw)
	winners := c.conflictWinners()
	ancestorRef := gatewayAncestorRef(gw)
	var errs []error

	for _, policy := range policies {
		relevant := policyTargetsUsedService(policy, used)
		updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := c.backendTLSPolicyLister.BackendTLSPolicies(policy.Namespace).Get(policy.Name)
			if err != nil {
				return err
			}
			updated := current.DeepCopy()
			if !relevant {
				before := len(updated.Status.Ancestors)
				removePolicyAncestor(&updated.Status, gatewayv1.GatewayController(controllerName), ancestorRef)
				if len(updated.Status.Ancestors) == before {
					return nil
				}
			} else {
				conflicted := policyIsConflicted(current, winners)
				_, caErr := c.resolveCACertificate(current)
				var tlsErr *backendTLSError
				if caErr != nil {
					var ok bool
					tlsErr, ok = caErr.(*backendTLSError)
					if !ok {
						tlsErr = &backendTLSError{
							reason:  gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef,
							message: caErr.Error(),
						}
					}
				}
				targetErr := c.inspectTargetRefs(current)
				conditions := backendTLSPolicyConditions(current.Generation, conflicted, tlsErr, targetErr)
				upsertPolicyAncestor(&updated.Status, gatewayv1.PolicyAncestorStatus{
					AncestorRef:    ancestorRef,
					ControllerName: gatewayv1.GatewayController(controllerName),
					Conditions:     conditions,
				})
			}

			if semanticIgnoreLastTransitionTime.DeepEqual(current.Status, updated.Status) {
				return nil
			}
			_, err = c.gwClient.GatewayV1().BackendTLSPolicies(updated.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
			return err
		})
		if updateErr != nil {
			errs = append(errs, fmt.Errorf("failed to update status for BackendTLSPolicy %s/%s: %w", policy.Namespace, policy.Name, updateErr))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("backend TLS policy status updates: %v", errs)
	}
	return nil
}
