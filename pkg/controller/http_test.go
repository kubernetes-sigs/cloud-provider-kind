package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func Test_firstSuccessfulProbe(t *testing.T) {
	reqCh := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("received connection ")
		close(reqCh)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	// use an address that is not likely to exist to avoid flakes
	addresses := []string{"https://127.0.1.201:12349", ts.URL}
	got, err := firstSuccessfulProbe(context.Background(), addresses)
	if err != nil {
		t.Errorf("firstSuccessfulProbe() error = %v", err)
		return
	}
	if got != ts.URL {
		t.Errorf("firstSuccessfulProbe() = %v, want %v", got, ts.URL)
	}

	select {
	case <-reqCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("test timed out, no request received")
	}

}
