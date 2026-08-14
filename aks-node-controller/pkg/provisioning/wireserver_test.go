package provisioning

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClient returns a Client pointed at the given test server URL with retries disabled and a
// short budget so tests never block.
func testClient(endpoint string) *Client {
	c := NewClient(endpoint, testAgent)
	c.RetryInterval = 0
	c.TotalRetryTimeout = 2 * time.Second
	c.ReadTimeout = time.Second
	return c
}

func TestPostHealthReadySuccess(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	require.NoError(t, c.postHealth(context.Background(), stateReady, "", ""))

	assert.JSONEq(t, `{"state":"Ready"}`, string(gotBody))
	assert.Equal(t, testAgent, gotHeaders.Get("x-ms-guest-agent-name"))
	assert.Equal(t, testAgent, gotHeaders.Get("User-Agent"))
	assert.Equal(t, "application/json", gotHeaders.Get("Content-Type"))
}

func TestPostHealthFailureBody(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	require.NoError(t, c.postHealth(context.Background(), stateNotReady, subStatusProvisioningFailed, "result=error|reason=boom"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &body))
	assert.Equal(t, "NotReady", body["state"])
	details, ok := body["details"].(map[string]any)
	require.True(t, ok, "details object must be present for a failure report")
	assert.Equal(t, "ProvisioningFailed", details["subStatus"])
	assert.Equal(t, "result=error|reason=boom", details["description"])
}

func TestPostHealthRetriesThenTimesOut(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.RetryInterval = 10 * time.Millisecond
	c.TotalRetryTimeout = 100 * time.Millisecond
	require.Error(t, c.postHealth(context.Background(), stateReady, "", ""))
	assert.GreaterOrEqual(t, calls.Load(), int32(2), "503 must be retried")
}

func TestPostHealthRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.RetryInterval = 5 * time.Millisecond
	c.TotalRetryTimeout = 5 * time.Second
	require.NoError(t, c.postHealth(context.Background(), stateReady, "", ""))
	assert.Equal(t, int32(3), calls.Load())
}

func TestPostHealthNonRetryableBails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.RetryInterval = 5 * time.Millisecond
	c.TotalRetryTimeout = 5 * time.Second
	require.Error(t, c.postHealth(context.Background(), stateReady, "", ""))
	assert.Equal(t, int32(1), calls.Load(), "400 must not be retried")
}

func TestPostHealthContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := testClient(srv.URL)
	c.RetryInterval = 50 * time.Millisecond
	c.TotalRetryTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, c.postHealth(ctx, stateReady, "", ""))
}

func TestIsRetryableStatus(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		assert.True(t, isRetryableStatus(code), "%d should be retryable", code)
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		assert.False(t, isRetryableStatus(code), "%d should not be retryable", code)
	}
}
