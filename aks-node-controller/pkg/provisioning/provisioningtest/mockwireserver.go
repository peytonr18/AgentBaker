// Package provisioningtest provides reusable test doubles for verifying provisioning health
// reporting: a mock wireserver that records the reports it receives, and a helper for decoding
// the Hyper-V KVP pool file.
package provisioningtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Report is a provisioning-health report parsed from a wireserver POST body.
type Report struct {
	State       string
	SubStatus   string
	Description string
}

// healthBody mirrors the JSON document the provisioning package POSTs to the wireserver.
type healthBody struct {
	State   string `json:"state"`
	Details *struct {
		SubStatus   string `json:"subStatus"`
		Description string `json:"description"`
	} `json:"details"`
}

// RecordingHandler is an http.Handler that records the provisioning-health reports it receives
// and replies with a configurable sequence of status codes (201 by default). Safe for concurrent
// use.
type RecordingHandler struct {
	mu       sync.Mutex
	reports  []Report
	statuses []int
	calls    int
}

// SetResponses sets the HTTP status codes returned to successive requests; once exhausted the
// last code repeats. With none set, 201 is returned. Useful for exercising retry/fatal paths.
func (h *RecordingHandler) SetResponses(statuses ...int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.statuses = statuses
}

func (h *RecordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/provisioning/health" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var parsed healthBody
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.State == "" {
		http.Error(w, "invalid health report", http.StatusBadRequest)
		return
	}
	report := Report{State: parsed.State}
	if parsed.Details != nil {
		report.SubStatus = parsed.Details.SubStatus
		report.Description = parsed.Details.Description
	}

	h.mu.Lock()
	h.reports = append(h.reports, report)
	status := http.StatusCreated
	if n := len(h.statuses); n > 0 {
		idx := h.calls
		if idx >= n {
			idx = n - 1
		}
		status = h.statuses[idx]
	}
	h.calls++
	h.mu.Unlock()

	w.WriteHeader(status)
}

// Reports returns a snapshot of the reports received so far.
func (h *RecordingHandler) Reports() []Report {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Report(nil), h.reports...)
}

// Calls returns the number of requests received.
func (h *RecordingHandler) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// MockWireserver is an in-process stand-in for the wireserver provisioning-health endpoint.
type MockWireserver struct {
	*RecordingHandler
	server *httptest.Server
}

// NewMockWireserver starts a mock wireserver. Call Close when finished.
func NewMockWireserver() *MockWireserver {
	handler := &RecordingHandler{}
	return &MockWireserver{RecordingHandler: handler, server: httptest.NewServer(handler)}
}

// URL returns the provisioning-health endpoint URL to pass as the report endpoint.
func (m *MockWireserver) URL() string {
	return m.server.URL + "/provisioning/health"
}

// Close shuts down the mock server.
func (m *MockWireserver) Close() {
	m.server.Close()
}
