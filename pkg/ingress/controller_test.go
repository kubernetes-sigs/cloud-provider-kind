package ingress

import (
	"context" // New import
	// New import
	"fmt"
	"reflect" // Import reflect for DeepEqual
	"strings" // Import standard library strings package
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr" // Added for pointer helpers

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	fakegateway "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

const (
	testGatewayClassName = "test-gw-class"
	testNamespace        = "test-ns"
	testIngressName      = "test-ingress"
	testServiceName      = "test-service"
	testServicePortName  = "http"
	testServicePortNum   = 8080
)

// testFixture holds all the components needed for a test
type testFixture struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	controller  *Controller
	kubeClient  *fakekube.Clientset
	gwClient    *fakegateway.Clientset
	kubeObjects []runtime.Object
	gwObjects   []runtime.Object
}

// newTestFixture creates a new test harness
func newTestFixture(t *testing.T, kubeObjects []runtime.Object, gwObjects []runtime.Object) *testFixture {
	ctx, cancel := context.WithCancel(context.Background())
	kubeClient := fakekube.NewSimpleClientset(kubeObjects...)
	gwClient := fakegateway.NewSimpleClientset(gwObjects...)

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, 0)
	gwInformerFactory := gatewayinformers.NewSharedInformerFactory(gwClient, 0)

	// Get informers
	ingInformer := kubeInformerFactory.Networking().V1().Ingresses()
	classInformer := kubeInformerFactory.Networking().V1().IngressClasses()
	svcInformer := kubeInformerFactory.Core().V1().Services()
	secretInformer := kubeInformerFactory.Core().V1().Secrets()
	routeInformer := gwInformerFactory.Gateway().V1().HTTPRoutes()
	gwInformer := gwInformerFactory.Gateway().V1().Gateways()

	// Create controller
	controller, err := NewController(
		ctx,
		kubeClient,
		gwClient,
		testGatewayClassName,
		ingInformer,
		classInformer,
		svcInformer,
		secretInformer,
		routeInformer,
		gwInformer,
	)
	if err != nil {
		t.Fatalf("Failed to create controller: %v", err)
	}

	// Start informers
	kubeInformerFactory.Start(ctx.Done())
	gwInformerFactory.Start(ctx.Done())

	// Wait for caches to sync
	ok := cache.WaitForCacheSync(ctx.Done(),
		ingInformer.Informer().HasSynced,
		classInformer.Informer().HasSynced,
		svcInformer.Informer().HasSynced,
		secretInformer.Informer().HasSynced,
		routeInformer.Informer().HasSynced,
		gwInformer.Informer().HasSynced,
	)
	if !ok {
		t.Fatalf("Failed to sync caches")
	}

	return &testFixture{
		t:           t,
		ctx:         ctx,
		cancel:      cancel,
		controller:  controller,
		kubeClient:  kubeClient,
		gwClient:    gwClient,
		kubeObjects: kubeObjects,
		gwObjects:   gwObjects,
	}
}

// --- Helper Objects ---

func newTestIngressClass() *networkingv1.IngressClass {
	return &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: IngressClassName,
		},
		Spec: networkingv1.IngressClassSpec{
			Controller: IngressClassController,
		},
	}
}

func newTestService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name: testServicePortName,
					Port: int32(testServicePortNum),
				},
				{
					Name: "other-port",
					Port: 8181,
				},
			},
		},
	}
}

func newTestIngress(name string) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ptr.To(IngressClassName),
			Rules: []networkingv1.IngressRule{
				{
					Host: "example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: testServiceName,
											Port: networkingv1.ServiceBackendPort{
												Name: testServicePortName, // Test port name translation
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// --- Test Cases ---

func TestSyncHandler_Create(t *testing.T) {
	// 1. Setup
	ingress := newTestIngress(testIngressName)
	service := newTestService(testServiceName)
	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingress, service, ingressClass}, // kube objects
		[]runtime.Object{}, // gateway objects
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. First Sync: Create Gateway, HTTPRoute. Fails on status update.
	err := f.controller.syncHandler(f.ctx, key)
	if err == nil {
		t.Fatalf("First sync should fail waiting for Gateway IP, but got nil error")
	}
	if !strings.Contains(err.Error(), "has no IP or Hostname address in status yet") {
		t.Errorf("Error message should indicate missing IP, but got: %v", err)
	}

	// 3. Assert Gateway was created
	gw, err := f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created Gateway: %v", err)
	}
	if string(gw.Spec.GatewayClassName) != testGatewayClassName {
		t.Errorf("Gateway Class: expected %q, got %q", testGatewayClassName, gw.Spec.GatewayClassName)
	}
	// With no Ingress TLS, Gateway should only have 1 listener (http)
	if len(gw.Spec.Listeners) != 1 {
		t.Errorf("Gateway Listeners: expected %d, got %d", 1, len(gw.Spec.Listeners))
	}
	if string(gw.Spec.Listeners[0].Name) != "http" {
		t.Errorf("Listener 0: expected %q, got %q", "http", gw.Spec.Listeners[0].Name)
	}

	// 4. Assert HTTPRoute was created (with the new name)
	expectedRouteName := generateRouteName(testIngressName, "example.com")
	route, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, expectedRouteName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created HTTPRoute: %v", err)
	}
	if route.Name != expectedRouteName {
		t.Errorf("HTTPRoute Name: expected %q, got %q", expectedRouteName, route.Name)
	}
	// Check Hostname
	if len(route.Spec.Hostnames) != 1 || route.Spec.Hostnames[0] != "example.com" {
		t.Errorf("HTTPRoute Hostnames: expected [\"example.com\"], got %v", route.Spec.Hostnames)
	}
	// Check ParentRef
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("HTTPRoute ParentRefs: expected %d, got %d", 1, len(route.Spec.ParentRefs))
	}
	if string(route.Spec.ParentRefs[0].Name) != GatewayName {
		t.Errorf("ParentRef Name: expected %q, got %q", GatewayName, route.Spec.ParentRefs[0].Name)
	}
	if route.Spec.ParentRefs[0].Namespace == nil || string(*route.Spec.ParentRefs[0].Namespace) != testNamespace {
		t.Errorf("ParentRef Namespace: expected %q, got %v", testNamespace, route.Spec.ParentRefs[0].Namespace)
	}
	// Check BackendRef (port name was translated to number)
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("HTTPRoute Rules: expected %d, got %d", 1, len(route.Spec.Rules))
	}
	if len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("HTTPRoute BackendRefs: expected %d, got %d", 1, len(route.Spec.Rules[0].BackendRefs))
	}
	if route.Spec.Rules[0].BackendRefs[0].Port == nil || int32(*route.Spec.Rules[0].BackendRefs[0].Port) != int32(testServicePortNum) {
		t.Errorf("BackendRef Port: expected %d, got %v", testServicePortNum, route.Spec.Rules[0].BackendRefs[0].Port)
	}

	// 5. Fake the Gateway Controller: Update Gateway status with a Hostname
	gw.Status.Addresses = []gatewayv1.GatewayStatusAddress{
		{
			Type:  ptr.To(gatewayv1.HostnameAddressType),
			Value: "my-test-lb.com",
		},
	}
	_, err = f.gwClient.GatewayV1().Gateways(testNamespace).UpdateStatus(f.ctx, gw, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update Gateway status: %v", err)
	}

	// 6. Second Sync: Update Ingress Status
	// We must use a poller here because the controller's informer needs to see the status update.
	// The handleGateway function will re-enqueue the Ingress.
	err = wait.PollUntilContextTimeout(f.ctx, 100*time.Millisecond, 5*time.Second, false, func(ctx context.Context) (bool, error) {
		// We call processNextWorkItem to simulate the worker picking up the key
		// that handleGateway enqueued.
		if f.controller.workqueue.Len() > 0 {
			f.controller.processNextWorkItem(ctx)
		}

		// Check if ingress status is updated
		ing, err := f.kubeClient.NetworkingV1().Ingresses(testNamespace).Get(ctx, testIngressName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if len(ing.Status.LoadBalancer.Ingress) > 0 {
			return ing.Status.LoadBalancer.Ingress[0].Hostname == "my-test-lb.com", nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Failed to update Ingress status with Gateway Hostname: %v", err)
	}
}

func TestSyncHandler_Delete(t *testing.T) {
	// 1. Setup: Start with an existing Gateway with TLS
	// This test verifies that when an Ingress is deleted, the Gateway is
	// reconciled (e.g., to remove TLS certs).
	// HTTPRoute deletion is handled by GC, so we don't test that here.

	// This Gateway has an HTTPS listener that should be removed
	existingGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GatewayName,
			Namespace: testNamespace,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: testGatewayClassName,
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				{ // This listener should be removed
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: ptr.To(gatewayv1.TLSModeTerminate),
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "old-secret"},
						},
					},
				},
			},
		},
	}
	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingressClass},    // kube objects
		[]runtime.Object{existingGateway}, // gateway objects
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync: Ingress is not found, so controller should reconcile the Gateway
	err := f.controller.syncHandler(f.ctx, key)
	if err != nil {
		t.Fatalf("SyncHandler should not error on delete, but got: %v", err)
	}

	// 3. Assert Gateway was updated to remove HTTPS listener
	gw, err := f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get Gateway: %v", err)
	}
	if len(gw.Spec.Listeners) != 1 {
		t.Errorf("Gateway Listeners: expected 1 (http only), got %d", len(gw.Spec.Listeners))
	}
	if gw.Spec.Listeners[0].Name != "http" {
		t.Errorf("Gateway listener should be http, got %s", gw.Spec.Listeners[0].Name)
	}
}

func TestSyncHandler_TranslationError_ServicePort(t *testing.T) {
	// 1. Setup: Ingress references a port name that doesn't exist on the service
	ingress := newTestIngress(testIngressName)
	service := newTestService(testServiceName)

	// Modify ingress to point to a bad port name
	ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name = "bad-port-name"
	ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number = 0

	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingress, service, ingressClass},
		[]runtime.Object{},
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync: Translation should fail
	err := f.controller.syncHandler(f.ctx, key)
	if err == nil {
		t.Fatalf("SyncHandler should error on bad port name, but got nil error")
	}
	if !strings.Contains(err.Error(), "port name bad-port-name not found in service") {
		t.Errorf("Error message should indicate missing port, but got: %v", err)
	}

	// 3. Assert HTTPRoute was NOT created
	expectedRouteName := generateRouteName(testIngressName, "example.com")
	_, err = f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, expectedRouteName, metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("HTTPRoute should not be created (IsNotFound), but got: %v", err)
	}
}
func TestSyncHandler_TLS_Aggregation(t *testing.T) {
	// 1. Setup
	service := newTestService(testServiceName)
	ingressClass := newTestIngressClass()

	// Ingress A specifies secret-a
	ingressA := newTestIngress("ingress-a")
	ingressA.Spec.TLS = []networkingv1.IngressTLS{
		{SecretName: "secret-a"},
	}

	// Ingress B specifies secret-b and a duplicate secret-a
	ingressB := newTestIngress("ingress-b")
	ingressB.Spec.Rules[0].Host = "b.example.com"
	ingressB.Spec.TLS = []networkingv1.IngressTLS{
		{SecretName: "secret-b"},
		{SecretName: "secret-a"}, // Duplicate
	}

	// Ingress C is not managed by us
	ingressC := newTestIngress("ingress-c")
	ingressC.Spec.Rules[0].Host = "c.example.com"
	ingressC.Spec.IngressClassName = ptr.To("other-class") // Different class
	ingressC.Spec.TLS = []networkingv1.IngressTLS{
		{SecretName: "secret-c"},
	}

	f := newTestFixture(t,
		[]runtime.Object{service, ingressClass, ingressA, ingressB, ingressC}, // kube objects
		[]runtime.Object{}, // gateway objects
	)
	defer f.cancel()

	keyA := fmt.Sprintf("%s/%s", testNamespace, "ingress-a")
	keyB := fmt.Sprintf("%s/%s", testNamespace, "ingress-b")

	// 2. Sync Ingress A
	// This sync will fail on status update, but that's OK.
	// We only care about the Gateway reconciliation.
	// The controller's lister will see ALL ingresses in the namespace.
	_ = f.controller.syncHandler(f.ctx, keyA)

	// 3. Assert Gateway is created with secret-a AND secret-b
	gw, err := f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created Gateway: %v", err)
	}

	// Gateway should now have 2 listeners
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("Gateway Listeners: expected 2, got %d", len(gw.Spec.Listeners))
	}
	httpsListener := gw.Spec.Listeners[1]
	if httpsListener.Name != "https" {
		t.Fatal("Listener 1 is not 'https'")
	}
	if httpsListener.TLS == nil || *httpsListener.TLS.Mode != gatewayv1.TLSModeTerminate {
		t.Errorf("TLS Mode: expected %q, got %v", gatewayv1.TLSModeTerminate, httpsListener.TLS)
	}

	// Secrets are sorted: secret-a, secret-b. secret-c is ignored.
	expectedCerts := []gatewayv1.SecretObjectReference{
		{Name: "secret-a"},
		{Name: "secret-b"},
	}
	if !reflect.DeepEqual(httpsListener.TLS.CertificateRefs, expectedCerts) {
		t.Errorf("Gateway certs: expected %v, got %v", expectedCerts, httpsListener.TLS.CertificateRefs)
	}

	// 4. Sync Ingress B
	// This sync should be a no-op for the Gateway, as state is already correct.
	_ = f.controller.syncHandler(f.ctx, keyB)
	gw, err = f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated Gateway: %v", err)
	}
	if !reflect.DeepEqual(gw.Spec.Listeners[1].TLS.CertificateRefs, expectedCerts) {
		t.Errorf("Gateway certs should be unchanged, but they were: %v", gw.Spec.Listeners[1].TLS.CertificateRefs)
	}

	// 5. Update Ingress A to remove its TLS spec
	ingressAUpdate, err := f.kubeClient.NetworkingV1().Ingresses(testNamespace).Get(f.ctx, "ingress-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get ingress-a for update: %v", err)
	}
	ingressAUpdate.Spec.TLS = nil
	_, err = f.kubeClient.NetworkingV1().Ingresses(testNamespace).Update(f.ctx, ingressAUpdate, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update ingress-a to remove TLS: %v", err)
	}

	// Wait for the informer to see the update
	time.Sleep(100 * time.Millisecond)

	// 6. Re-sync Ingress A (keyA)
	// This sync will now see only Ingress B has TLS.
	_ = f.controller.syncHandler(f.ctx, keyA)

	// 7. Assert Gateway is UPDATED, now only with secret-a and secret-b (from Ingress B)
	gw, err = f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated Gateway: %v", err)
	}

	// Ingress B still holds both "secret-a" and "secret-b"
	expectedCerts = []gatewayv1.SecretObjectReference{
		{Name: "secret-a"},
		{Name: "secret-b"},
	}
	if len(gw.Spec.Listeners) != 2 { // Still 2 listeners, cert list is unchanged
		t.Errorf("Gateway listener count: expected 2, got %d", len(gw.Spec.Listeners))
	}
	if !reflect.DeepEqual(gw.Spec.Listeners[1].TLS.CertificateRefs, expectedCerts) {
		t.Errorf("Gateway certs: expected %v, got %v", expectedCerts, gw.Spec.Listeners[1].TLS.CertificateRefs)
	}

	// 8. Update Ingress B to remove its TLS
	ingressBUpdate, err := f.kubeClient.NetworkingV1().Ingresses(testNamespace).Get(f.ctx, "ingress-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get ingress-b for update: %v", err)
	}
	ingressBUpdate.Spec.TLS = nil
	_, err = f.kubeClient.NetworkingV1().Ingresses(testNamespace).Update(f.ctx, ingressBUpdate, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("Failed to update ingress-b to remove TLS: %v", err)
	}

	// Wait for the informer to see the update
	time.Sleep(100 * time.Millisecond)

	// 9. Re-sync Ingress B (keyB)
	// This sync will now see *zero* ingresses with TLS.
	_ = f.controller.syncHandler(f.ctx, keyB)

	// 10. Assert Gateway is UPDATED again, now with *no* HTTPS listener
	gw, err = f.gwClient.GatewayV1().Gateways(testNamespace).Get(f.ctx, GatewayName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get updated Gateway: %v", err)
	}

	if len(gw.Spec.Listeners) != 1 {
		t.Errorf("Gateway listener count: expected 1 (http only), got %d", len(gw.Spec.Listeners))
	}
	if gw.Spec.Listeners[0].Name != "http" {
		t.Errorf("Gateway listener should be http, got %s", gw.Spec.Listeners[0].Name)
	}
}

// === NEW TEST ===
func TestSyncHandler_DefaultBackend(t *testing.T) {
	// 1. Setup: Ingress with ONLY spec.defaultBackend
	ingress := newTestIngress(testIngressName)
	ingress.Spec.Rules = nil // No rules
	ingress.Spec.DefaultBackend = &networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: testServiceName,
			Port: networkingv1.ServiceBackendPort{Number: int32(testServicePortNum)},
		},
	}

	service := newTestService(testServiceName)
	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingress, service, ingressClass},
		[]runtime.Object{},
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync
	// This will fail on status update, which is fine
	_ = f.controller.syncHandler(f.ctx, key)

	// 3. Assert HTTPRoute was created for the default backend
	expectedRouteName := generateRouteName(testIngressName, "") // "" host
	route, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, expectedRouteName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created HTTPRoute for default backend: %v", err)
	}

	// 4. Assert Hostnames is nil (meaning "all hosts")
	if route.Spec.Hostnames != nil {
		t.Errorf("Default backend route should have nil Hostnames, got %v", route.Spec.Hostnames)
	}

	// 5. Assert Rule is correct
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("Default backend route Rules: expected 1, got %d", len(route.Spec.Rules))
	}
	if route.Spec.Rules[0].Matches != nil {
		t.Errorf("Default backend rule Matches: expected nil, got %v", route.Spec.Rules[0].Matches)
	}
	if len(route.Spec.Rules[0].BackendRefs) != 1 {
		t.Fatalf("Default backend rule BackendRefs: expected 1, got %d", len(route.Spec.Rules[0].BackendRefs))
	}
	backendRef := route.Spec.Rules[0].BackendRefs[0]
	if string(backendRef.Name) != testServiceName {
		t.Errorf("BackendRef Name: expected %s, got %s", testServiceName, backendRef.Name)
	}
	if *backendRef.Port != int32(testServicePortNum) {
		t.Errorf("BackendRef Port: expected %d, got %d", testServicePortNum, *backendRef.Port)
	}
}

// === NEW TEST ===
func TestSyncHandler_DefaultRulePrecedence(t *testing.T) {
	// 1. Setup: Ingress with BOTH spec.defaultBackend and a host: "" rule.
	// The host: "" rule should win.
	serviceA := newTestService("service-a")
	serviceB := newTestService("service-b")

	ingress := newTestIngress(testIngressName)
	ingress.Spec.Rules = []networkingv1.IngressRule{
		{
			Host: "", // Default rule
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: ptr.To(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "service-b", // This one should be used
									Port: networkingv1.ServiceBackendPort{Name: testServicePortName},
								},
							},
						},
					},
				},
			},
		},
	}
	ingress.Spec.DefaultBackend = &networkingv1.IngressBackend{
		Service: &networkingv1.IngressServiceBackend{
			Name: "service-a", // This one should be ignored
			Port: networkingv1.ServiceBackendPort{Number: int32(testServicePortNum)},
		},
	}

	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingress, serviceA, serviceB, ingressClass},
		[]runtime.Object{},
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync
	_ = f.controller.syncHandler(f.ctx, key)

	// 3. Assert HTTPRoute was created
	expectedRouteName := generateRouteName(testIngressName, "") // "" host
	route, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, expectedRouteName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get created HTTPRoute for default backend: %v", err)
	}

	// 4. Assert Hostnames is nil
	if route.Spec.Hostnames != nil {
		t.Errorf("Default backend route should have nil Hostnames, got %v", route.Spec.Hostnames)
	}

	// 5. Assert Rule points to service-b (proving precedence)
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("Default backend route Rules: expected 1, got %d", len(route.Spec.Rules))
	}
	backendRef := route.Spec.Rules[0].BackendRefs[0]
	if string(backendRef.Name) != "service-b" {
		t.Errorf("BackendRef Name: expected 'service-b', got %s", backendRef.Name)
	}
}

// === NEW TEST ===
func TestSyncHandler_MultipleHostRules(t *testing.T) {
	// 1. Setup: One Ingress, two host rules.
	serviceA := newTestService("service-a")
	serviceB := newTestService("service-b")
	ingressClass := newTestIngressClass()

	ingress := newTestIngress(testIngressName)
	ingress.Spec.Rules = []networkingv1.IngressRule{
		{
			Host: "a.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: ptr.To(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "service-a",
									Port: networkingv1.ServiceBackendPort{Name: testServicePortName},
								},
							},
						},
					},
				},
			},
		},
		{
			Host: "b.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: ptr.To(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "service-b",
									Port: networkingv1.ServiceBackendPort{Name: testServicePortName},
								},
							},
						},
					},
				},
			},
		},
	}

	f := newTestFixture(t,
		[]runtime.Object{ingress, serviceA, serviceB, ingressClass},
		[]runtime.Object{},
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync
	_ = f.controller.syncHandler(f.ctx, key)

	// 3. Assert Route A was created
	routeNameA := generateRouteName(testIngressName, "a.example.com")
	routeA, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, routeNameA, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get route for a.example.com: %v", err)
	}
	if len(routeA.Spec.Hostnames) != 1 || routeA.Spec.Hostnames[0] != "a.example.com" {
		t.Errorf("Route A Hostnames: expected [\"a.example.com\"], got %v", routeA.Spec.Hostnames)
	}
	if string(routeA.Spec.Rules[0].BackendRefs[0].Name) != "service-a" {
		t.Errorf("Route A Backend: expected 'service-a', got %s", routeA.Spec.Rules[0].BackendRefs[0].Name)
	}

	// 4. Assert Route B was created
	routeNameB := generateRouteName(testIngressName, "b.example.com")
	routeB, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, routeNameB, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get route for b.example.com: %v", err)
	}
	if len(routeB.Spec.Hostnames) != 1 || routeB.Spec.Hostnames[0] != "b.example.com" {
		t.Errorf("Route B Hostnames: expected [\"b.example.com\"], got %v", routeB.Spec.Hostnames)
	}
	if string(routeB.Spec.Rules[0].BackendRefs[0].Name) != "service-b" {
		t.Errorf("Route B Backend: expected 'service-b', got %s", routeB.Spec.Rules[0].BackendRefs[0].Name)
	}
}

// === NEW TEST ===
func TestSyncHandler_Update_StaleRouteDeletion(t *testing.T) {
	// 1. Setup:
	// - An Ingress for "a.example.com"
	// - A pre-existing, stale HTTPRoute for "b.example.com" that is *owned*
	//   by this Ingress (simulating a removed host rule).
	ingress := newTestIngress(testIngressName)
	ingress.Spec.Rules = []networkingv1.IngressRule{
		{
			Host: "a.example.com", // The ONLY desired host
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: ptr.To(networkingv1.PathTypePrefix),
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: testServiceName,
									Port: networkingv1.ServiceBackendPort{Name: testServicePortName},
								},
							},
						},
					},
				},
			},
		},
	}

	staleRouteName := generateRouteName(testIngressName, "b.example.com")
	staleRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      staleRouteName,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(ingress, networkingv1.SchemeGroupVersion.WithKind("Ingress")),
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"b.example.com"},
		},
	}

	service := newTestService(testServiceName)
	ingressClass := newTestIngressClass()

	f := newTestFixture(t,
		[]runtime.Object{ingress, service, ingressClass},
		[]runtime.Object{staleRoute}, // Pre-create the stale route
	)
	defer f.cancel()

	key := fmt.Sprintf("%s/%s", testNamespace, testIngressName)

	// 2. Sync
	_ = f.controller.syncHandler(f.ctx, key)

	// 3. Assert desired route ("a") was created
	desiredRouteName := generateRouteName(testIngressName, "a.example.com")
	_, err := f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, desiredRouteName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get desired route: %v", err)
	}

	// 4. Assert stale route ("b") was deleted
	_, err = f.gwClient.GatewayV1().HTTPRoutes(testNamespace).Get(f.ctx, staleRouteName, metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Stale HTTPRoute should be deleted (IsNotFound), but got: %v", err)
	}
}
