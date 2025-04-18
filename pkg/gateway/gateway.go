package gateway

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (c *Controller) processNextGatewayItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.gatewayqueue.Get()
	if quit {
		return false
	}
	defer c.gatewayqueue.Done(key)

	err := c.syncGateway(key)

	c.handleGatewayErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncGateway(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing gateway %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.gatewayLister.Gateways(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if apierrors.IsNotFound(err) {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		fmt.Printf("Gateway %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		fmt.Printf("Sync/Add/Update for Gateway %s\n", gw.GetName())
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleGatewayErr(err error, key string) {
	if err == nil {
		c.gatewayqueue.Forget(key)
		return
	}

	if c.gatewayqueue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing Gateway %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.gatewayqueue.AddRateLimited(key)
		return
	}

	c.gatewayqueue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping Gateway %q out of the queue: %v", key, err)
}

func (c *Controller) runGatewayWorker(ctx context.Context) {
	for c.processNextGatewayItem() {
	}
}
