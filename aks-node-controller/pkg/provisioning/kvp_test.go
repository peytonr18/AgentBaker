package provisioning

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestEncodeKVPRecordShortValue(t *testing.T) {
	record, err := encodeKVPRecord("k", "v")
	require.NoError(t, err)
	require.Len(t, record, kvpRecordBytes)
	assert.Equal(t, byte('k'), record[0])
	assert.Equal(t, byte(0), record[1], "key is zero-padded")
	assert.Equal(t, byte('v'), record[kvpMaxKeyBytes], "value starts after the key field")
	assert.Equal(t, byte(0), record[kvpMaxKeyBytes+1], "value is zero-padded")
}

func TestEncodeKVPRecordRejectsLongValue(t *testing.T) {
	_, err := encodeKVPRecord("k", strings.Repeat("A", kvpChunkBytes+1))
	assert.Error(t, err)
}

func TestEncodeKVPRecordRejectsLongKey(t *testing.T) {
	_, err := encodeKVPRecord(strings.Repeat("k", kvpMaxKeyBytes+1), "v")
	assert.Error(t, err)
}

func TestEncodeKVPEntrySingleRecordForSmallValue(t *testing.T) {
	records, err := encodeKVPEntry("k", "v")
	require.NoError(t, err)
	require.Len(t, records, 1)
	k, v := decodeKVPRecord(t, records[0])
	assert.Equal(t, "k", k, "a value that fits stays under the bare key")
	assert.Equal(t, "v", v)
}

func TestEncodeKVPEntryChunksLargeValue(t *testing.T) {
	value := strings.Repeat("A", kvpChunkBytes) + strings.Repeat("B", 50)
	records, err := encodeKVPEntry("k", value)
	require.NoError(t, err)
	require.Len(t, records, 2)
	var got string
	for i, r := range records {
		k, v := decodeKVPRecord(t, r)
		assert.Equal(t, fmt.Sprintf("k%s%d", kvpChunkKeySeparator, i), k, "chunk i is keyed k|i")
		got += v
	}
	assert.Equal(t, value, got, "concatenating chunks in order restores the value")
}

func TestKVPUpsertAndDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }
	require.NoError(t, w.upsert([]kvpEntry{{key: "mykey", value: "myvalue"}}))

	records := readKVPFile(t, path)
	require.Len(t, records, 1)
	k, v := decodeKVPRecord(t, records[0])
	assert.Equal(t, "mykey", k)
	assert.Equal(t, "myvalue", v)
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

func TestKVPWriteProvisioningReportUpsertsExistingKeys(t *testing.T) {
	orig := lookupDmesg
	t.Cleanup(func() { lookupDmesg = orig })
	lookupDmesg = func() ([]byte, error) { return []byte("new kernel log"), nil }

	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }
	legacyEntries := []kvpEntry{
		{key: "unrelated", value: "preserved"},
		{key: kvpDmesgKey, value: "old kernel log"},
		{key: kvpProvisioningReportKey, value: "result=error"},
		{key: kvpProvisioningReportKey, value: "duplicate"},
	}
	var legacyPool []byte
	for _, entry := range legacyEntries {
		record, err := encodeKVPRecord(entry.key, entry.value)
		require.NoError(t, err)
		legacyPool = append(legacyPool, record...)
	}
	require.NoError(t, os.WriteFile(path, legacyPool, 0o600))
	require.NoError(t, w.WriteProvisioningReport("result=success"))

	got := map[string]string{}
	for _, record := range readKVPFile(t, path) {
		key, value := decodeKVPRecord(t, record)
		assert.NotContains(t, got, key, "upsert must leave one record per replaced key")
		got[key] = value
	}
	assert.Equal(t, "preserved", got["unrelated"])
	assert.Equal(t, "new kernel log", got[kvpDmesgKey])
	assert.Equal(t, "result=success", got[kvpProvisioningReportKey])
}

func TestKVPWriteProvisioningReportSerializesConcurrentWriters(t *testing.T) {
	orig := lookupDmesg
	t.Cleanup(func() { lookupDmesg = orig })
	lookupDmesg = func() ([]byte, error) { return []byte("kernel log"), nil }

	w := NewKVPWriter(filepath.Join(t.TempDir(), ".kvp_pool_1"))
	w.bootTime = func() (int64, error) { return 0, nil }
	const writers = 8
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errs <- w.WriteProvisioningReport(fmt.Sprintf("result=success|writer=%d", index))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	keys := map[string]int{}
	for _, record := range readKVPFile(t, w.Path) {
		key, _ := decodeKVPRecord(t, record)
		keys[key]++
	}
	assert.Equal(t, 1, keys[kvpDmesgKey])
	assert.Equal(t, 1, keys[kvpProvisioningReportKey])
}

func TestKVPWriteProvisioningReportChunksOversizedReport(t *testing.T) {
	origDmesg := lookupDmesg
	t.Cleanup(func() { lookupDmesg = origDmesg })
	lookupDmesg = func() ([]byte, error) { return nil, fmt.Errorf("no dmesg") }

	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }
	report := "result=error|reason=" + strings.Repeat("x", kvpChunkBytes*2)
	require.NoError(t, w.WriteProvisioningReport(report))

	records := readKVPFile(t, path)
	require.Greater(t, len(records), 1, "oversized report must be split into chunk records")
	chunks := map[int]string{}
	for _, r := range records {
		k, v := decodeKVPRecord(t, r)
		suffix, ok := strings.CutPrefix(k, kvpProvisioningReportKey+kvpChunkKeySeparator)
		require.True(t, ok, "chunk key %q must be %s|<index>", k, kvpProvisioningReportKey)
		idx, err := strconv.Atoi(suffix)
		require.NoError(t, err)
		chunks[idx] = v
	}
	var got string
	for i := 0; i < len(chunks); i++ {
		got += chunks[i]
	}
	assert.Equal(t, report, got, "concatenating chunks in index order must restore the report")
}

func TestKVPWriteProvisioningReportReplacesChunksWithSingleRecord(t *testing.T) {
	origDmesg := lookupDmesg
	t.Cleanup(func() { lookupDmesg = origDmesg })
	lookupDmesg = func() ([]byte, error) { return nil, fmt.Errorf("no dmesg") }

	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }

	require.NoError(t, w.WriteProvisioningReport(strings.Repeat("x", kvpChunkBytes*2)))
	require.Greater(t, len(readKVPFile(t, path)), 1, "oversized report should produce multiple chunk records")

	require.NoError(t, w.WriteProvisioningReport("result=success"))
	records := readKVPFile(t, path)
	require.Len(t, records, 1, "a small report must replace the prior chunk records")
	k, v := decodeKVPRecord(t, records[0])
	assert.Equal(t, kvpProvisioningReportKey, k)
	assert.Equal(t, "result=success", v)
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
	w.bootTime = func() (int64, error) { return 0, nil }
	require.NoError(t, w.upsert([]kvpEntry{{key: "old", value: "data"}}))
	require.Len(t, readKVPFile(t, path), 1)

	// Boot time is after the file mtime → stale → truncated.
	w.bootTime = func() (int64, error) { return time.Now().Add(time.Hour).Unix(), nil }
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, w.truncateIfStale(f))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Zero(t, info.Size())
}

func TestKVPTruncateIfStaleKeepsFreshData(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }
	require.NoError(t, w.upsert([]kvpEntry{{key: "fresh", value: "data"}}))

	// Boot time is before the file mtime → fresh → kept.
	w.bootTime = func() (int64, error) { return time.Now().Add(-time.Hour).Unix(), nil }
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, w.truncateIfStale(f))
	assert.Len(t, readKVPFile(t, path), 1)
}

func TestKVPUpsertRejectsMalformedPool(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kvp_pool_1")
	require.NoError(t, os.WriteFile(path, []byte("partial"), 0o600))
	w := NewKVPWriter(path)
	w.bootTime = func() (int64, error) { return 0, nil }
	assert.Error(t, w.upsert([]kvpEntry{{key: "k", value: "v"}}))
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
