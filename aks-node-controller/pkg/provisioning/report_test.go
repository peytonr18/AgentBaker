package provisioning

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testVMID  = "00000000-0000-0000-0000-000000000abc"
	testAgent = "AKS-CSE"
	testTS    = "2026-06-17T00:00:00Z"
)

func TestEncodeSuccess(t *testing.T) {
	r := NewSuccessReport(testAgent, testVMID, PpsTypeNone)
	r.timestamp = testTS
	assert.Equal(t,
		"result=success|agent=AKS-CSE|pps_type=None|vm_id=00000000-0000-0000-0000-000000000abc|timestamp=2026-06-17T00:00:00Z",
		r.Encode())
}

func TestEncodeSuccessWithExtras(t *testing.T) {
	// Success layout appends extras at the very end, in insertion order.
	r := NewSuccessReport(testAgent, testVMID, PpsTypeNone).
		WithExtra("endpoint", "http://example.com").
		WithExtra("status", "404")
	r.timestamp = testTS
	assert.Equal(t,
		"result=success|agent=AKS-CSE|pps_type=None|vm_id=00000000-0000-0000-0000-000000000abc|timestamp=2026-06-17T00:00:00Z|endpoint=http://example.com|status=404",
		r.Encode())
}

func TestEncodeFailure(t *testing.T) {
	r := NewFailureReport(testAgent, testVMID, "boom", PpsTypeNone).
		WithDocumentationURL(DocumentationURL)
	r.timestamp = testTS
	assert.Equal(t,
		"result=error|reason=boom|agent=AKS-CSE|pps_type=None|vm_id=00000000-0000-0000-0000-000000000abc|"+
			"timestamp=2026-06-17T00:00:00Z|documentation_url=https://aka.ms/linuxprovisioningerror",
		r.Encode())
}

func TestEncodeFailureWithExtras(t *testing.T) {
	// Failure layout: result, reason, agent, extras, pps_type, vm_id, timestamp, documentation_url.
	r := NewFailureReport(testAgent, testVMID, "boom", PpsTypeNone).
		WithExtra("details", "bad config").
		WithDocumentationURL(DocumentationURL)
	r.timestamp = testTS
	assert.Equal(t,
		"result=error|reason=boom|agent=AKS-CSE|details=bad config|pps_type=None|"+
			"vm_id=00000000-0000-0000-0000-000000000abc|timestamp=2026-06-17T00:00:00Z|"+
			"documentation_url=https://aka.ms/linuxprovisioningerror",
		r.Encode())
}

func TestEncodeQuotesDelimiter(t *testing.T) {
	// A field containing the pipe delimiter must be quoted as a whole (CSV Necessary),
	// matching azure-init's csv-based encode_report.
	r := NewSuccessReport(testAgent, testVMID, PpsTypeNone).WithExtra("k", "a|b")
	r.timestamp = testTS
	require.Contains(t, r.Encode(), `"k=a|b"`)
}

func TestDefaultTimestampPopulated(t *testing.T) {
	r := NewSuccessReport(testAgent, testVMID, PpsTypeNone)
	assert.NotEmpty(t, r.timestamp)
}
