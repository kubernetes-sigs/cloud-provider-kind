package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

func probeHTTP(ctx context.Context, address string) bool {
	klog.Infof("probe HTTP address %s", address)
	httpClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequest("GET", address, nil)
	if err != nil {
		return false
	}
	req = req.WithContext(ctx)
	resp, err := httpClient.Do(req)
	if err != nil {
		klog.Infof("Failed to connect to HTTP address %s: %v", address, err)
		return false
	}
	defer resp.Body.Close()
	// drain the body
	io.ReadAll(resp.Body) // nolint:errcheck
	// we only want to verify connectivity so don't need to check the http status code
	// as the apiserver may not be ready
	return true
}

// firstSuccessfulProbe probes the given addresses in parallel and returns the first address to succeed, cancelling the other probes.
func firstSuccessfulProbe(ctx context.Context, addresses []string) (string, error) {
	var wg sync.WaitGroup
	resultChan := make(chan string, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, addr := range addresses {
		wg.Add(1)
		go func(address string) {
			defer wg.Done()
			if probeHTTP(ctx, address) {
				select {
				case resultChan <- address:
				default:
				}
				cancel()
			}
		}(addr)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	select {
	case result := <-resultChan:
		return result, nil
	case <-ctx.Done():
		return "", fmt.Errorf("no address succeeded")
	}
}
