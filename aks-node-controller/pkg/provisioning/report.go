package provisioning

import (
	"bytes"
	"encoding/csv"
	"strings"
	"time"
)

// AgentName identifies the reporting agent. It is emitted in the encoded report's `agent`
// field and in the wireserver request headers. It matches the AGENT_NAME used by the legacy
// python report-ready workaround so telemetry stays continuous across the rewrite.
const AgentName = "AKS-CSE"

// DocumentationURL is attached to failure reports to point operators at provisioning-error
// guidance. It mirrors azure-init's Error::DOCUMENTATION_URL.
const DocumentationURL = "https://aka.ms/linuxprovisioningerror"

// ReportResult is the provisioning outcome recorded in the encoded report's `result` field.
type ReportResult string

const (
	// ResultSuccess indicates provisioning completed successfully.
	ResultSuccess ReportResult = "success"
	// ResultError indicates provisioning failed.
	ResultError ReportResult = "error"
)

// PpsType is the pre-provisioning (PPS) type recorded in the `pps_type` field. It mirrors
// azure-init's ReportPpsType. Only PpsTypeNone is emitted today; the other values exist so the
// wire vocabulary matches azure-init if pre-provisioning support is added later.
type PpsType string

const (
	// PpsTypeNone means the node is not pre-provisioned.
	PpsTypeNone PpsType = "None"
	// PpsTypeOsDisk means a pre-provisioned OS disk.
	PpsTypeOsDisk PpsType = "PreprovisionedOSDisk"
	// PpsTypeRunning means running pre-provisioning.
	PpsTypeRunning PpsType = "Running"
	// PpsTypeSavable means savable pre-provisioning.
	PpsTypeSavable PpsType = "Savable"
	// PpsTypeUnknown means an unknown pre-provisioning type.
	PpsTypeUnknown PpsType = "Unknown"
)

// reportExtra is an ordered key/value pair of supporting data appended to a report.
type reportExtra struct {
	key   string
	value string
}

// ProvisioningReport is a strongly-typed provisioning health report. Construct one with
// NewSuccessReport or NewFailureReport, optionally attach context with the builder methods,
// then serialize it with Encode. It is the Go port of azure-init's ProvisioningReport
// (libazureinit-kvp/src/report.rs) and Error::as_encoded_report (libazureinit/src/error.rs).
type ProvisioningReport struct {
	result           ReportResult
	agent            string
	vmID             string
	timestamp        string
	ppsType          PpsType
	reason           string // present for failure reports
	documentationURL string // present for failure reports
	extra            []reportExtra
}

// NewSuccessReport creates a success report stamped with the current time (RFC 3339, UTC).
func NewSuccessReport(agent, vmID string, ppsType PpsType) *ProvisioningReport {
	return &ProvisioningReport{
		result:    ResultSuccess,
		agent:     agent,
		vmID:      vmID,
		timestamp: time.Now().UTC().Format(time.RFC3339),
		ppsType:   ppsType,
	}
}

// NewFailureReport creates a failure report with a reason, stamped with the current time.
func NewFailureReport(agent, vmID, reason string, ppsType PpsType) *ProvisioningReport {
	return &ProvisioningReport{
		result:    ResultError,
		agent:     agent,
		vmID:      vmID,
		timestamp: time.Now().UTC().Format(time.RFC3339),
		ppsType:   ppsType,
		reason:    reason,
	}
}

// WithDocumentationURL attaches a documentation URL (failure reports). It returns the report
// so calls can be chained.
func (r *ProvisioningReport) WithDocumentationURL(url string) *ProvisioningReport {
	r.documentationURL = url
	return r
}

// WithExtra appends an ordered key/value pair of supporting data. Extras are emitted in the
// order they were added. It returns the report so calls can be chained.
func (r *ProvisioningReport) WithExtra(key, value string) *ProvisioningReport {
	r.extra = append(r.extra, reportExtra{key: key, value: value})
	return r
}

// Encode serializes the report as a single pipe-delimited key=value string, matching the exact
// field ordering azure-init uses so the Azure/Hyper-V host parses it identically:
//   - success: result, agent, pps_type, vm_id, timestamp, then extras in insertion order.
//   - failure: result, reason, agent, extras in insertion order, pps_type, vm_id, timestamp,
//     then documentation_url (if set).
func (r *ProvisioningReport) Encode() string {
	fields := make([]string, 0, 7+len(r.extra))
	fields = append(fields, "result="+string(r.result))
	switch r.result {
	case ResultSuccess:
		fields = append(fields, "agent="+r.agent)
		fields = append(fields, "pps_type="+string(r.ppsType))
		fields = append(fields, "vm_id="+r.vmID)
		fields = append(fields, "timestamp="+r.timestamp)
		for _, e := range r.extra {
			fields = append(fields, e.key+"="+e.value)
		}
	case ResultError:
		if r.reason != "" {
			fields = append(fields, "reason="+r.reason)
		}
		fields = append(fields, "agent="+r.agent)
		for _, e := range r.extra {
			fields = append(fields, e.key+"="+e.value)
		}
		fields = append(fields, "pps_type="+string(r.ppsType))
		fields = append(fields, "vm_id="+r.vmID)
		fields = append(fields, "timestamp="+r.timestamp)
		if r.documentationURL != "" {
			fields = append(fields, "documentation_url="+r.documentationURL)
		}
	}
	return encodeFields(fields)
}

// encodeFields joins fields with a pipe delimiter using CSV quoting semantics that match
// azure-init's encode_report (csv writer, delimiter '|', QuoteStyle::Necessary): a field is
// wrapped in double quotes only when it contains the delimiter, a quote, or a newline.
func encodeFields(fields []string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Comma = '|'
	// Writing a single record to an in-memory buffer cannot fail.
	_ = w.Write(fields)
	w.Flush()
	return strings.TrimRight(buf.String(), "\n")
}
