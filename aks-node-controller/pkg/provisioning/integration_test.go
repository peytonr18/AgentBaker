package provisioning_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning"
	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning/provisioningtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testVMID = "00000000-0000-0000-0000-000000000abc"

// fastClient returns a provisioning client aimed at the mock, with retries that don't slow tests.
func fastClient(endpoint string) *provisioning.Client {
	c := provisioning.NewClient(endpoint, "")
	c.RetryInterval = 5 * time.Millisecond
	c.TotalRetryTimeout = 2 * time.Second
	c.ReadTimeout = time.Second
	return c
}

func TestReportReadyDeliversReadyReport(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()

	err := provisioning.ReportReady(context.Background(), provisioning.Options{
		Client:     fastClient(mock.URL()),
		VMID:       testVMID,
		DisableKVP: true,
	})
	require.NoError(t, err)

	reports := mock.Reports()
	require.Len(t, reports, 1)
	assert.Equal(t, "Ready", reports[0].State)
	assert.Empty(t, reports[0].SubStatus)
}

func TestReportFailureDeliversFailureReport(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()

	err := provisioning.ReportFailure(context.Background(), provisioning.Options{
		Client:     fastClient(mock.URL()),
		VMID:       testVMID,
		DisableKVP: true,
	}, "cse failed with exit code 42")
	require.NoError(t, err)

	reports := mock.Reports()
	require.Len(t, reports, 1)
	assert.Equal(t, "NotReady", reports[0].State)
	assert.Equal(t, "ProvisioningFailed", reports[0].SubStatus)
	assert.Contains(t, reports[0].Description, "result=error|reason=cse failed with exit code 42|agent=AKS-CSE")
	assert.Contains(t, reports[0].Description, "vm_id="+testVMID)
}

func TestReportRetriesThenSucceeds(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()
	mock.SetResponses(http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusCreated)

	err := provisioning.ReportReady(context.Background(), provisioning.Options{
		Client:     fastClient(mock.URL()),
		VMID:       testVMID,
		DisableKVP: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, mock.Calls())
}

func TestReportFatalStatusBails(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()
	mock.SetResponses(http.StatusBadRequest)

	err := provisioning.ReportReady(context.Background(), provisioning.Options{
		Client:     fastClient(mock.URL()),
		VMID:       testVMID,
		DisableKVP: true,
	})
	require.Error(t, err)
	assert.Equal(t, 1, mock.Calls(), "a 400 response must not be retried")
}

func TestReportReadyWritesReportAndKVP(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()
	kvpPath := filepath.Join(t.TempDir(), ".kvp_pool_1")

	err := provisioning.ReportReady(context.Background(), provisioning.Options{
		Client:  fastClient(mock.URL()),
		VMID:    testVMID,
		KVPPath: kvpPath,
	})
	require.NoError(t, err)

	// Delivered to the wireserver.
	require.Len(t, mock.Reports(), 1)
	assert.Equal(t, "Ready", mock.Reports()[0].State)

	// And persisted to the KVP pool.
	records, err := provisioningtest.ReadKVPPool(kvpPath)
	require.NoError(t, err)
	value, found, err := provisioningtest.ReassembleKVPValue(records, "PROVISIONING_REPORT")
	require.NoError(t, err)
	require.True(t, found, "PROVISIONING_REPORT record must be present in the KVP pool")
	assert.Contains(t, value, "result=success")
	assert.Contains(t, value, "vm_id="+testVMID)
}
