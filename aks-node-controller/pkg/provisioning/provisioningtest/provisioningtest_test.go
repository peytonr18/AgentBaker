package provisioningtest

import (
	"os"
	"path/filepath"
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

	value, found := FindKVPValue(records, "PROVISIONING_REPORT")
	assert.True(t, found)
	assert.Equal(t, "result=success", value)

	_, found = FindKVPValue(records, "missing")
	assert.False(t, found)
}
