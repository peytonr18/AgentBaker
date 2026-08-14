package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Azure/agentbaker/aks-node-controller/common"
)

const (
	// DefaultHealthEndpoint is the wireserver provisioning-health endpoint azure-init POSTs to.
	// It is a plain-HTTP link-local endpoint; unlike the legacy WALinuxAgent/cloud-init path it
	// needs no GoalState fetch and takes a small JSON document rather than XML.
	DefaultHealthEndpoint = "http://168.63.129.16/provisioning/health"

	// DefaultConnectionTimeout bounds the TCP connect for a single attempt.
	DefaultConnectionTimeout = 60 * time.Second
	// DefaultReadTimeout bounds a single request/response attempt.
	DefaultReadTimeout = 60 * time.Second
	// DefaultTotalRetryTimeout bounds the whole retry budget. azure-init retries the wireserver
	// for up to 20 minutes because the VM has most likely failed provisioning past that point.
	DefaultTotalRetryTimeout = 20 * time.Minute
	// DefaultRetryInterval is the pause between attempts.
	DefaultRetryInterval = 5 * time.Second
)

// provisioningState is the wireserver health state ("Ready"/"NotReady").
type provisioningState string

const (
	stateReady    provisioningState = "Ready"
	stateNotReady provisioningState = "NotReady"
)

// provisioningSubStatus is the health detail sub-status.
type provisioningSubStatus string

const (
	subStatusProvisioningFailed provisioningSubStatus = "ProvisioningFailed"
)

// healthDetails is the optional "details" object in the health JSON body.
type healthDetails struct {
	SubStatus   string `json:"subStatus"`
	Description string `json:"description"`
}

// healthBody is the JSON document POSTed to the wireserver health endpoint. It matches the
// shape azure-init builds: {"state":"Ready"} or
// {"state":"NotReady","details":{"subStatus":"...","description":"..."}}.
type healthBody struct {
	State   string         `json:"state"`
	Details *healthDetails `json:"details,omitempty"`
}

// Client posts provisioning-health reports to the wireserver.
type Client struct {
	// Endpoint is the wireserver health endpoint. Defaults to DefaultHealthEndpoint.
	Endpoint string
	// Agent identifies the guest agent in the request headers.
	Agent string
	// HTTPClient performs the POST. Defaults to a fail-fast plain-HTTP client.
	HTTPClient *http.Client
	// RetryInterval is the pause between attempts.
	RetryInterval time.Duration
	// ReadTimeout bounds a single attempt.
	ReadTimeout time.Duration
	// TotalRetryTimeout bounds the whole retry budget.
	TotalRetryTimeout time.Duration
}

// NewClient builds a Client with azure-init-aligned defaults. endpoint and agent may be empty
// to use DefaultHealthEndpoint and AgentName respectively.
func NewClient(endpoint, agent string) *Client {
	if endpoint == "" {
		endpoint = DefaultHealthEndpoint
	}
	if agent == "" {
		agent = AgentName
	}
	transport := common.NewBaseTransport(common.HTTPTransportOptions{
		DialTimeout:           DefaultConnectionTimeout,
		TLSHandshakeTimeout:   DefaultConnectionTimeout,
		ResponseHeaderTimeout: DefaultReadTimeout,
	})
	return &Client{
		Endpoint:          endpoint,
		Agent:             agent,
		HTTPClient:        &http.Client{Transport: transport},
		RetryInterval:     DefaultRetryInterval,
		ReadTimeout:       DefaultReadTimeout,
		TotalRetryTimeout: DefaultTotalRetryTimeout,
	}
}

// attemptOutcome classifies the result of a single postHealth attempt.
type attemptOutcome int

const (
	attemptSucceeded attemptOutcome = iota
	attemptRetryable
	attemptFatal
)

// postHealth builds the JSON body and POSTs it, retrying transient failures until the total
// retry budget is exhausted. It follows azure-init health.rs::_report semantics:
//   - success on any 2xx.
//   - retry on 429/500/503 and transport/connection errors, pausing RetryInterval between attempts.
//   - bail immediately on any other non-2xx (e.g. 400/401/403/404).
func (c *Client) postHealth(ctx context.Context, state provisioningState, subStatus provisioningSubStatus, description string) error {
	body := healthBody{State: string(state)}
	if subStatus != "" {
		body.Details = &healthDetails{SubStatus: string(subStatus), Description: description}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling health body: %w", err)
	}

	deadline := time.Now().Add(c.TotalRetryTimeout)
	var lastErr error
	for attempt := 1; ; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptTimeout := c.ReadTimeout
		if attemptTimeout <= 0 || attemptTimeout > remaining {
			attemptTimeout = remaining
		}

		outcome, attemptErr := c.postAttempt(ctx, payload, attemptTimeout)
		switch outcome {
		case attemptSucceeded:
			slog.Info("wireserver health report succeeded", "state", state, "attempt", attempt)
			return nil
		case attemptFatal:
			return fmt.Errorf("reporting %q to wireserver: %w", state, attemptErr)
		case attemptRetryable:
			lastErr = attemptErr
			slog.Warn("wireserver health report attempt failed, will retry", "state", state, "attempt", attempt, "error", attemptErr)
		}

		if ctx.Err() != nil {
			return fmt.Errorf("reporting %q to wireserver cancelled: %w", state, ctx.Err())
		}
		// Stop if there is no time left for another attempt after pausing.
		if c.RetryInterval >= time.Until(deadline) {
			break
		}
		if err := sleepWithContext(ctx, c.RetryInterval); err != nil {
			return fmt.Errorf("reporting %q to wireserver cancelled: %w", state, err)
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no report attempt completed within the retry budget")
	}
	return fmt.Errorf("reporting %q to wireserver failed within %s: %w", state, c.TotalRetryTimeout, lastErr)
}

// postAttempt performs a single POST and classifies the outcome per azure-init health.rs::_report:
// any 2xx succeeds, 429/500/503 and transport errors are retryable, and any other status is fatal.
func (c *Client) postAttempt(ctx context.Context, payload []byte, timeout time.Duration) (attemptOutcome, error) {
	statusCode, err := c.doPost(ctx, payload, timeout)
	switch {
	case err != nil:
		return attemptRetryable, err
	case statusCode >= 200 && statusCode < 300:
		return attemptSucceeded, nil
	case isRetryableStatus(statusCode):
		return attemptRetryable, fmt.Errorf("wireserver returned retryable status %d", statusCode)
	default:
		return attemptFatal, fmt.Errorf("wireserver returned non-retryable status %d", statusCode)
	}
}

// sleepWithContext pauses for d, returning early with ctx.Err() if the context is cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doPost performs a single bounded POST attempt and returns the HTTP status code. A transport
// error (connection refused, timeout, etc.) is returned as a non-nil error with a zero status.
func (c *Client) doPost(ctx context.Context, payload []byte, timeout time.Duration) (int, error) {
	attemptCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", c.Agent)
	req.Header.Set("x-ms-guest-agent-name", c.Agent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drain the body so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// isRetryableStatus reports whether an HTTP status warrants a retry. It follows azure-init
// health.rs::_report: only 429, 500 and 503 are retryable; every other non-2xx status is fatal.
// (azure-init's generic http.rs RETRY_CODES additionally lists 400/404/410, but the health path
// deliberately narrows retries to transient server-side conditions.)
func isRetryableStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}
