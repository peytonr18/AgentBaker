package main

import (
	"context"
	"testing"

	"github.com/Azure/agentbaker/aks-node-controller/pkg/provisioning/provisioningtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunReportReady(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()

	err := run(context.Background(), []string{
		"provisioning-report", "report-ready",
		"--endpoint", mock.URL(),
		"--disable-kvp",
		"--total-retry-timeout", "2s",
	})
	require.NoError(t, err)
	require.Len(t, mock.Reports(), 1)
	assert.Equal(t, "Ready", mock.Reports()[0].State)
}

func TestRunReportFailure(t *testing.T) {
	mock := provisioningtest.NewMockWireserver()
	defer mock.Close()

	err := run(context.Background(), []string{
		"provisioning-report", "report-failure",
		"--endpoint", mock.URL(),
		"--reason", "boom",
		"--disable-kvp",
		"--total-retry-timeout", "2s",
	})
	require.NoError(t, err)
	require.Len(t, mock.Reports(), 1)
	assert.Equal(t, "NotReady", mock.Reports()[0].State)
	assert.Contains(t, mock.Reports()[0].Description, "reason=boom")
}

func TestParseSupportingData(t *testing.T) {
	got, err := parseSupportingData([]string{"a=1", "b=2"})
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"a", "1"}, {"b", "2"}}, got)

	_, err = parseSupportingData([]string{"noequals"})
	assert.Error(t, err)
}
