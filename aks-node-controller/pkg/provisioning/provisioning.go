// Package provisioning reports AKS node provisioning health to the Azure wireserver.
//
// It is a Go port of azure-init's health reporting (libazureinit/src/health.rs, report.rs and
// error.rs). Provisioning success ("Ready") or failure ("NotReady"/"ProvisioningFailed") is
// reported to the wireserver provisioning-health endpoint using the modern JSON protocol that
// azure-init uses: a single POST of a small JSON document to
// http://168.63.129.16/provisioning/health. Unlike the legacy WALinuxAgent/cloud-init protocol
// it does not fetch the GoalState or build XML.
//
// The package exposes ReportReady and ReportFailure for the standalone provisioning-report CLI
// and future Go callers. Each operation also writes the encoded report and dmesg diagnostics to
// the Hyper-V KVP pool on a best-effort basis.
package provisioning

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// vmIDProductUUIDPath is the DMI system-UUID file used as the VM ID. Reading it needs no
	// privileges and works pre-network.
	vmIDProductUUIDPath = "/sys/class/dmi/id/product_uuid"

	// zeroVMID is the all-zero fallback used when the VM ID cannot be determined, matching the
	// legacy python report-ready workaround.
	zeroVMID = "00000000-0000-0000-0000-000000000000"
)

// lookupVMIDFromDMIDecode resolves the VM ID via `dmidecode -s system-uuid`. It is a package
// variable so tests can stub the subprocess call.
//
//nolint:gochecknoglobals // test seam for the dmidecode fallback
var lookupVMIDFromDMIDecode = func() (string, error) {
	out, err := exec.Command("dmidecode", "-s", "system-uuid").Output()
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(string(out))), nil
}

// Options configures a report call.
type Options struct {
	// Endpoint overrides the wireserver health endpoint. Empty uses DefaultHealthEndpoint.
	Endpoint string
	// Agent overrides the reporting agent identifier. Empty uses AgentName.
	Agent string
	// VMID overrides the VM ID. Empty reads it from the DMI product_uuid (with fallbacks).
	VMID string
	// TotalRetryTimeout overrides the retry budget. Zero uses DefaultTotalRetryTimeout.
	TotalRetryTimeout time.Duration
	// DocumentationURL overrides the failure documentation URL. Empty uses DocumentationURL.
	DocumentationURL string
	// SupportingData is appended to the report as ordered key/value extras.
	SupportingData [][2]string
	// DisableKVP disables writing the provisioning report and dmesg to the Hyper-V KVP pool.
	// KVP writes are best-effort diagnostics and never block or fail the wireserver report.
	DisableKVP bool
	// KVPPath overrides the KVP pool file path. Empty uses the default guest pool file.
	KVPPath string
	// Client overrides the wireserver client, primarily for tests. When nil a default client is
	// built from Endpoint/Agent/TotalRetryTimeout.
	Client *Client
	// vmIDPath overrides the DMI product_uuid path (tests only).
	vmIDPath string
}

func (o Options) client() *Client {
	if o.Client != nil {
		return o.Client
	}
	c := NewClient(o.Endpoint, o.Agent)
	if o.TotalRetryTimeout > 0 {
		c.TotalRetryTimeout = o.TotalRetryTimeout
	}
	return c
}

func (o Options) agent() string {
	if o.Agent != "" {
		return o.Agent
	}
	return AgentName
}

func (o Options) vmID() string {
	if o.VMID != "" {
		return o.VMID
	}
	return getVMID(o.vmIDPath)
}

// writeKVP writes the encoded report and dmesg diagnostics to the Hyper-V KVP pool. Failures are
// logged and swallowed so a diagnostic-channel failure never blocks the wireserver report.
func (o Options) writeKVP(report string) {
	if o.DisableKVP {
		return
	}
	if err := NewKVPWriter(o.KVPPath).WriteProvisioningReport(report); err != nil {
		slog.Warn("failed to write provisioning report to KVP (continuing)", "error", err)
	}
}

// ReportReady reports provisioning success ("Ready") to the wireserver. It POSTs
// {"state":"Ready"} and returns an error only if the report cannot be delivered within the
// retry budget.
func ReportReady(ctx context.Context, opts Options) error {
	c := opts.client()
	vmID := opts.vmID()

	// The encoded success report is written to KVP but is not part of the Ready HTTP body,
	// matching azure-init.
	report := NewSuccessReport(opts.agent(), vmID, PpsTypeNone)
	for _, kv := range opts.SupportingData {
		report.WithExtra(kv[0], kv[1])
	}
	encoded := report.Encode()
	slog.Info("reporting provisioning ready", "vm_id", vmID, "endpoint", c.Endpoint, "report", encoded)
	opts.writeKVP(encoded)

	return c.postHealth(ctx, stateReady, "", "")
}

// ReportFailure reports provisioning failure ("NotReady"/"ProvisioningFailed") to the
// wireserver. The encoded failure report is sent as the health detail description, matching
// azure-init. It returns an error only if the report cannot be delivered within the retry
// budget; successfully reporting a failure is not itself an error.
func ReportFailure(ctx context.Context, opts Options, reason string) error {
	c := opts.client()
	vmID := opts.vmID()

	docURL := opts.DocumentationURL
	if docURL == "" {
		docURL = DocumentationURL
	}
	report := NewFailureReport(opts.agent(), vmID, reason, PpsTypeNone)
	for _, kv := range opts.SupportingData {
		report.WithExtra(kv[0], kv[1])
	}
	report.WithDocumentationURL(docURL)
	encoded := report.Encode()
	slog.Info("reporting provisioning failure", "vm_id", vmID, "endpoint", c.Endpoint, "reason", reason, "report", encoded)
	opts.writeKVP(encoded)

	return c.postHealth(ctx, stateNotReady, subStatusProvisioningFailed, encoded)
}

// getVMID returns the VM ID (DMI system UUID), lower-cased. It reads the product_uuid file,
// falling back to `dmidecode -s system-uuid`, then to the all-zero UUID. Failures are logged
// rather than returned because a missing VM ID must not block reporting.
func getVMID(path string) string {
	if path == "" {
		path = vmIDProductUUIDPath
	}
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.ToLower(strings.TrimSpace(string(data))); id != "" {
			return id
		}
	} else {
		slog.Warn("failed to read VM ID from product_uuid, trying dmidecode", "path", path, "error", err)
	}
	if id, err := lookupVMIDFromDMIDecode(); err == nil && id != "" {
		return id
	}
	slog.Warn("could not determine VM ID; using all-zero UUID")
	return zeroVMID
}
