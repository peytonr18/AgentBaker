package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeVMIDFile(t *testing.T, id string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "product_uuid")
	require.NoError(t, os.WriteFile(p, []byte(id+"\n"), 0o600))
	return p
}

func TestReportReadyEndToEnd(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := ReportReady(context.Background(), Options{Client: testClient(srv.URL), vmIDPath: writeVMIDFile(t, testVMID), DisableKVP: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"Ready"}`, string(gotBody))
}

func TestReportFailureEndToEnd(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := ReportFailure(context.Background(),
		Options{Client: testClient(srv.URL), vmIDPath: writeVMIDFile(t, testVMID), DisableKVP: true},
		"cse failed with exit code 42")
	require.NoError(t, err)

	var body struct {
		State   string `json:"state"`
		Details struct {
			SubStatus   string `json:"subStatus"`
			Description string `json:"description"`
		} `json:"details"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &body))
	assert.Equal(t, "NotReady", body.State)
	assert.Equal(t, "ProvisioningFailed", body.Details.SubStatus)
	assert.Contains(t, body.Details.Description, "result=error|reason=cse failed with exit code 42|agent=AKS-CSE")
	assert.Contains(t, body.Details.Description, "vm_id="+testVMID)
	assert.Contains(t, body.Details.Description, "documentation_url="+DocumentationURL)
}

func TestReportFailureSupportingData(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := ReportFailure(context.Background(),
		Options{
			Client:         testClient(srv.URL),
			vmIDPath:       writeVMIDFile(t, testVMID),
			SupportingData: [][2]string{{"exit_code", "42"}},
			DisableKVP:     true,
		},
		"cse failed")
	require.NoError(t, err)

	var body struct {
		Details struct {
			Description string `json:"description"`
		} `json:"details"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &body))
	assert.Contains(t, body.Details.Description, "exit_code=42")
}

func TestGetVMIDFromFile(t *testing.T) {
	// getVMID lower-cases the DMI UUID.
	p := writeVMIDFile(t, "ABCDEF00-0000-0000-0000-000000000ABC")
	assert.Equal(t, "abcdef00-0000-0000-0000-000000000abc", getVMID(p))
}

func TestGetVMIDFallsBackToDMIDecode(t *testing.T) {
	orig := lookupVMIDFromDMIDecode
	t.Cleanup(func() { lookupVMIDFromDMIDecode = orig })
	lookupVMIDFromDMIDecode = func() (string, error) { return "dmidecode-uuid", nil }

	assert.Equal(t, "dmidecode-uuid", getVMID(filepath.Join(t.TempDir(), "missing")))
}

func TestGetVMIDFallsBackToZero(t *testing.T) {
	orig := lookupVMIDFromDMIDecode
	t.Cleanup(func() { lookupVMIDFromDMIDecode = orig })
	lookupVMIDFromDMIDecode = func() (string, error) { return "", fmt.Errorf("dmidecode unavailable") }

	assert.Equal(t, zeroVMID, getVMID(filepath.Join(t.TempDir(), "missing")))
}

func TestReportReadyWritesKVP(t *testing.T) {
	origDmesg := lookupDmesg
	t.Cleanup(func() { lookupDmesg = origDmesg })
	lookupDmesg = func() ([]byte, error) { return []byte("boot log"), nil }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	kvpPath := filepath.Join(t.TempDir(), ".kvp_pool_1")
	err := ReportReady(context.Background(), Options{
		Client:   testClient(srv.URL),
		vmIDPath: writeVMIDFile(t, testVMID),
		KVPPath:  kvpPath,
	})
	require.NoError(t, err)

	data, err := os.ReadFile(kvpPath)
	require.NoError(t, err)
	require.Positive(t, len(data))
	// The report and dmesg diagnostics must both be present in the pool file.
	assert.Contains(t, string(data), kvpProvisioningReportKey)
	assert.Contains(t, string(data), "result=success")
	assert.Contains(t, string(data), "boot log")
}
