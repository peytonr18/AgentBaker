package provisioningtest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadKVPPoolDecodesRecords(t *testing.T) {
	record := make([]byte, kvpRecordBytes)
	copy(record, "PROVISIONING_REPORT")
	copy(record[kvpKeyBytes:], "result=success")
	path := filepath.Join(t.TempDir(), "pool")
	require.NoError(t, os.WriteFile(path, record, 0o600))

	records, err := ReadKVPPool(path)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "PROVISIONING_REPORT", records[0].Key)
	assert.Equal(t, "result=success", records[0].Value)

	value, found, err := FindKVPValue(records, "PROVISIONING_REPORT")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "result=success", value)

	_, found, err = FindKVPValue(records, "missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestReadKVPPoolRejectsPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool")
	require.NoError(t, os.WriteFile(path, []byte("partial"), 0o600))
	_, err := ReadKVPPool(path)
	assert.ErrorContains(t, err, "not a multiple")
}

func TestFindKVPValueRejectsDuplicates(t *testing.T) {
	_, _, err := FindKVPValue([]KVPRecord{{Key: "k", Value: "first"}, {Key: "k", Value: "second"}}, "k")
	assert.ErrorContains(t, err, "duplicate key")
}

func TestReassembleKVPValueFromChunks(t *testing.T) {
	records := []KVPRecord{
		{Key: "OTHER", Value: "keep"},
		{Key: "PROVISIONING_REPORT|1", Value: "world"},
		{Key: "PROVISIONING_REPORT|0", Value: "hello "},
	}
	value, found, err := ReassembleKVPValue(records, "PROVISIONING_REPORT")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "hello world", value, "chunks reassemble in index order")
}

func TestReassembleKVPValuePrefersVerbatimRecord(t *testing.T) {
	records := []KVPRecord{{Key: "PROVISIONING_REPORT", Value: "result=success"}}
	value, found, err := ReassembleKVPValue(records, "PROVISIONING_REPORT")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "result=success", value)
}

func TestReassembleKVPValueMissingChunkFails(t *testing.T) {
	records := []KVPRecord{{Key: "PROVISIONING_REPORT|1", Value: "world"}}
	_, _, err := ReassembleKVPValue(records, "PROVISIONING_REPORT")
	assert.ErrorContains(t, err, "missing KVP chunk")
}

func TestRecordingHandlerRejectsInvalidRequests(t *testing.T) {
	handler := &RecordingHandler{}

	request := httptest.NewRequest(http.MethodGet, "/provisioning/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusMethodNotAllowed, response.Code)

	request = httptest.NewRequest(http.MethodPost, "/wrong", nil)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusNotFound, response.Code)

	request = httptest.NewRequest(http.MethodPost, "/provisioning/health", strings.NewReader("not-json"))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Empty(t, handler.Reports())
}
