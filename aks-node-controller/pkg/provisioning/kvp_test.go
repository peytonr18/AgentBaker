package provisioning

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeKVPRecord splits a fixed-width record into its key and value, trimming zero padding.
// Test-only: production values never contain embedded null bytes.
func decodeKVPRecord(t *testing.T, record []byte) (string, string) {
	t.Helper()
	require.Len(t, record, kvpRecordBytes)
	key := strings.TrimRight(string(record[:kvpMaxKeyBytes]), "\x00")
	value := strings.TrimRight(string(record[kvpMaxKeyBytes:]), "\x00")
	return key, value
}

// readKVPFile reads a pool file and splits it into fixed-width records.
func readKVPFile(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Zero(t, len(data)%kvpRecordBytes, "file size must be a multiple of the record size")
	var records [][]byte
	for i := 0; i < len(data); i += kvpRecordBytes {
		records = append(records, data[i:i+kvpRecordBytes])
	}
	return records
}

func TestEncodeKVPRecordsShortValue(t *testing.T) {
	records := encodeKVPRecords("k", "v")
	require.Len(t, records, 1)
	require.Len(t, records[0], kvpRecordBytes)
	assert.Equal(t, byte('k'), records[0][0])
	assert.Equal(t, byte(0), records[0][1], "key is zero-padded")
	assert.Equal(t, byte('v'), records[0][kvpMaxKeyBytes], "value starts after the key field")
	assert.Equal(t, byte(0), records[0][kvpMaxKeyBytes+1], "value is zero-padded")
}

func TestEncodeKVPRecordsChunksLongValue(t *testing.T) {
	value := strings.Repeat("A", kvpChunkBytes*2+50)
	records := encodeKVPRecords("k", value)
	require.Len(t, records, 3, "1022 + 1022 + 50")
	for _, r := range records {
		k, _ := decodeKVPRecord(t, r)
		assert.Equal(t, "k", k, "each chunk shares the key")
	}
	_, lastValue := decodeKVPRecord(t, records[2])
	assert.Len(t, lastValue, 50)
}

func TestEncodeKVPRecordsEmptyValue(t *testing.T) {
	assert.Empty(t, encodeKVPRecords("k", ""))
}

func TestEncodeKVPRecordsTruncatesLongKey(t *testing.T) {
	records := encodeKVPRecords(strings.Repeat("k", kvpMaxKeyBytes+10), "v")
	require.Len(t, records, 1)
	key, _ := decodeKVPRecord(t, records[0])
	assert.Len(t, key, kvpMaxKeyBytes, "key is truncated to the field width")
}

func TestKVPAppendAndDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	require.NoError(t, NewKVPWriter(path).append("mykey", "myvalue"))

	records := readKVPFile(t, path)
	require.Len(t, records, 1)
	k, v := decodeKVPRecord(t, records[0])
	assert.Equal(t, "mykey", k)
	assert.Equal(t, "myvalue", v)
}

func TestKVPAppendEmptyValueWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	require.NoError(t, NewKVPWriter(path).append("k", ""))
	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist, "an empty value must not create the pool file")
}

func TestKVPWriteProvisioningReport(t *testing.T) {
	orig := lookupDmesg
	t.Cleanup(func() { lookupDmesg = orig })
	lookupDmesg = func() ([]byte, error) { return []byte("kernel: hello"), nil }

	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	require.NoError(t, NewKVPWriter(path).WriteProvisioningReport("result=success|agent=AKS-CSE"))

	got := map[string]string{}
	for _, r := range readKVPFile(t, path) {
		k, v := decodeKVPRecord(t, r)
		got[k] = v
	}
	assert.Equal(t, "kernel: hello", got[kvpDmesgKey])
	assert.Equal(t, "result=success|agent=AKS-CSE", got[kvpProvisioningReportKey])
}

func TestKVPWriteProvisioningReportSurvivesDmesgFailure(t *testing.T) {
	orig := lookupDmesg
	t.Cleanup(func() { lookupDmesg = orig })
	lookupDmesg = func() ([]byte, error) { return nil, fmt.Errorf("dmesg unavailable") }

	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	require.NoError(t, NewKVPWriter(path).WriteProvisioningReport("result=success"))

	records := readKVPFile(t, path)
	require.Len(t, records, 1, "only the report record; dmesg was skipped")
	k, v := decodeKVPRecord(t, records[0])
	assert.Equal(t, kvpProvisioningReportKey, k)
	assert.Equal(t, "result=success", v)
}

func TestKVPTruncateIfStaleTruncatesOldData(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	require.NoError(t, w.append("old", "data"))
	require.Len(t, readKVPFile(t, path), 1)

	// Boot time is after the file mtime → stale → truncated.
	w.bootTime = func() (int64, error) { return time.Now().Add(time.Hour).Unix(), nil }
	require.NoError(t, w.truncateIfStale())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestKVPTruncateIfStaleKeepsFreshData(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	require.NoError(t, w.append("fresh", "data"))

	// Boot time is before the file mtime → fresh → kept.
	w.bootTime = func() (int64, error) { return time.Now().Add(-time.Hour).Unix(), nil }
	require.NoError(t, w.truncateIfStale())
	assert.Len(t, readKVPFile(t, path), 1)
}

func TestKVPTruncateIfStaleMissingFileIsOK(t *testing.T) {
	w := NewKVPWriter(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.NoError(t, w.truncateIfStale())
}

func TestParseBootTime(t *testing.T) {
	got, err := parseBootTime("cpu 1 2 3\nbtime 1710000000\nintr 5\n")
	require.NoError(t, err)
	assert.Equal(t, int64(1710000000), got)

	_, err = parseBootTime("cpu 1 2 3\n")
	assert.Error(t, err, "missing btime")

	_, err = parseBootTime("btime not-a-number\n")
	assert.Error(t, err, "invalid btime")
}
