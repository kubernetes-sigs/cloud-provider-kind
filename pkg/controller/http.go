package controller

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

func probeHTTP(ctx context.Context, address string) bool {
	klog.Infof("probe HTTP address %s", address)
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
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
