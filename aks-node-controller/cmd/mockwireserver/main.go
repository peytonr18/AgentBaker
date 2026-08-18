// Command mockwireserver runs an in-process stand-in for the Azure wireserver provisioning-health
// endpoint, logging every report it receives. It is a manual/e2e aid for exercising
// provisioning-report and aks-node-controller reporting: point their --endpoint at
// http://<addr>/provisioning/health.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning/provisioningtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8899", "address to listen on")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	handler := &provisioningtest.RecordingHandler{}
	slog.Info("mock wireserver listening", "addr", *addr, "path", "/provisioning/health")

	server := &http.Server{
		Addr:              *addr,
		Handler:           logReports(handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		slog.Error("mock wireserver stopped", "error", err)
		os.Exit(1)
	}
}

// logReports wraps the recording handler to log each received report as it arrives.
func logReports(h *provisioningtest.RecordingHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		reports := h.Reports()
		if len(reports) > 0 {
			last := reports[len(reports)-1]
			slog.Info("received provisioning report",
				"state", last.State, "subStatus", last.SubStatus, "description", last.Description)
		}
	}
}
