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

func (c *Controller) processNextGRPCrouteItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.grpcroutequeue.Get()
	if quit {
		return false
	}
	defer c.grpcroutequeue.Done(key)

	err := c.syncGRPCroute(key)

	c.handleGRPCrouteErr(err, key)
	return true
}

// syncToStdout is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) syncGRPCroute(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing HTTP route %q (%v)", key, time.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	gw, err := c.grpcrouteLister.GRPCRoutes(namespace).Get(name)
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Fetching object with key %s from store failed with %v", key, err)
		return err
	}

	if apierrors.IsNotFound(err) {
		// Below we will warm up our cache with a Pod, so that we will see a delete for one pod
		fmt.Printf("GRPCroute %s does not exist anymore\n", key)
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		fmt.Printf("Sync/Add/Update for GRPCroute %s\n", gw.GetName())
	}
	return nil
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleGRPCrouteErr(err error, key string) {
	if err == nil {
		c.grpcroutequeue.Forget(key)
		return
	}

	if c.grpcroutequeue.NumRequeues(key) < maxRetries {
		klog.Infof("Error syncing GRPCroute %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.grpcroutequeue.AddRateLimited(key)
		return
	}

	c.grpcroutequeue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping GRPCroute %q out of the queue: %v", key, err)
}

func (c *Controller) runGRPCrouteWorker(ctx context.Context) {
	for c.processNextGRPCrouteItem() {
	}
}
