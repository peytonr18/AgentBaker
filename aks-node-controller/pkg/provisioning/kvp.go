package provisioning

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Hyper-V KVP on-disk wire format. Keys and values are stored as zero-padded UTF-8 in
// fixed-width records, matching the Linux kernel HV_KVP_EXCHANGE_MAX_* sizes that
// hv_kvp_daemon exchanges. Mirrors azure-init (libazureinit/src/kvp.rs).
const (
	// kvpMaxKeyBytes is the fixed key field width (HV_KVP_EXCHANGE_MAX_KEY_SIZE).
	kvpMaxKeyBytes = 512
	// kvpMaxValueBytes is the fixed value field width (HV_KVP_EXCHANGE_MAX_VALUE_SIZE).
	kvpMaxValueBytes = 2048
	// kvpChunkBytes is the maximum value bytes packed per record before splitting into
	// additional same-key records (HV_KVP_AZURE_MAX_VALUE_SIZE). azure-init uses 1022 (two
	// bytes under 1024) to stay clear of the kernel value limit.
	kvpChunkBytes = 1022
	// kvpRecordBytes is the total fixed record size on disk.
	kvpRecordBytes = kvpMaxKeyBytes + kvpMaxValueBytes

	// kvpProvisioningReportKey is the key the Azure/Hyper-V host parses for the health report.
	kvpProvisioningReportKey = "PROVISIONING_REPORT"
	// kvpDmesgKey is the key under which recent kernel log output is stored for diagnostics.
	kvpDmesgKey = "dmesg"

	// defaultKVPPoolPath is the guest-to-host KVP pool file (.kvp_pool_1) where cloud-init /
	// azure-init write.
	defaultKVPPoolPath = "/var/lib/hyperv/.kvp_pool_1"
	// procStatPath is read to determine the system boot time (btime).
	procStatPath = "/proc/stat"
)

// lookupDmesg returns recent kernel log output. It is a package variable so tests can stub the
// subprocess call.
//
//nolint:gochecknoglobals // test seam for the dmesg capture
var lookupDmesg = func() ([]byte, error) {
	return exec.Command("dmesg").Output()
}

// KVPWriter appends provisioning telemetry to the Hyper-V guest KVP pool file so the
// Azure/Hyper-V host can read it. It mirrors azure-init's libazureinit kvp.rs runtime path:
// records use the fixed-width 512+2048 wire format, oversized values are chunked at 1022 bytes,
// records are appended under an exclusive lock, and the pool file is truncated once per boot if
// it holds stale data from a previous boot.
type KVPWriter struct {
	// Path is the KVP pool file. Defaults to /var/lib/hyperv/.kvp_pool_1.
	Path string
	// bootTime returns the system boot time in unix seconds; overridable for tests.
	bootTime func() (int64, error)
}

// NewKVPWriter builds a KVPWriter. path may be empty to use defaultKVPPoolPath.
func NewKVPWriter(path string) *KVPWriter {
	if path == "" {
		path = defaultKVPPoolPath
	}
	return &KVPWriter{Path: path, bootTime: bootTimeFromProcStat}
}

// WriteProvisioningReport truncates stale pool data (once per boot), appends recent dmesg output
// for diagnostics (best-effort), then appends the encoded provisioning report. dmesg failures do
// not prevent the report from being written.
func (w *KVPWriter) WriteProvisioningReport(report string) error {
	if err := w.truncateIfStale(); err != nil {
		slog.Warn("failed to truncate stale KVP pool file", "path", w.Path, "error", err)
	}
	if err := w.appendDmesg(); err != nil {
		slog.Warn("failed to write dmesg to KVP (continuing)", "error", err)
	}
	return w.append(kvpProvisioningReportKey, report)
}

// appendDmesg captures dmesg output and appends its tail under the dmesg key, matching the legacy
// python workaround's diagnostics.
func (w *KVPWriter) appendDmesg() error {
	out, err := lookupDmesg()
	if err != nil {
		return fmt.Errorf("running dmesg: %w", err)
	}
	// Keep only the tail that fits a single KVP value chunk.
	if len(out) > kvpChunkBytes {
		out = out[len(out)-kvpChunkBytes:]
	}
	// Sanitize to valid UTF-8 so the record round-trips through a UTF-8 decoder.
	return w.append(kvpDmesgKey, strings.ToValidUTF8(string(out), "\uFFFD"))
}

// append encodes key/value into one or more fixed-width records and appends them under an
// exclusive lock so it cooperates with hv_kvp_daemon and other writers.
func (w *KVPWriter) append(key, value string) error {
	records := encodeKVPRecords(key, value)
	if len(records) == 0 {
		return nil
	}

	// #nosec G304 -- the pool path is an operator/config value, not untrusted input.
	f, err := os.OpenFile(w.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening KVP pool file %s: %w", w.Path, err)
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking KVP pool file: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	for _, record := range records {
		if _, err := f.Write(record); err != nil {
			return fmt.Errorf("writing KVP record: %w", err)
		}
	}
	return nil
}

// truncateIfStale clears the pool file if its mtime predates the current boot, so telemetry from
// a previous boot does not accumulate. A missing file is not an error.
func (w *KVPWriter) truncateIfStale() error {
	info, err := os.Stat(w.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat KVP pool file: %w", err)
	}
	bootTime, err := w.bootTime()
	if err != nil {
		return fmt.Errorf("determining boot time: %w", err)
	}
	if info.ModTime().Unix() < bootTime {
		if err := os.Truncate(w.Path, 0); err != nil {
			return fmt.Errorf("truncating stale KVP pool file: %w", err)
		}
	}
	return nil
}

// encodeKVPRecords encodes key/value into fixed-width 512+2048 records, splitting values longer
// than kvpChunkBytes into multiple records that share the key. An empty value yields no records,
// matching azure-init's chunk-based encoding.
func encodeKVPRecords(key, value string) [][]byte {
	valueBytes := []byte(value)
	if len(valueBytes) == 0 {
		return nil
	}

	keyBuf := make([]byte, kvpMaxKeyBytes)
	copy(keyBuf, key) // copy truncates to the key field width.

	var records [][]byte
	for start := 0; start < len(valueBytes); start += kvpChunkBytes {
		end := min(start+kvpChunkBytes, len(valueBytes))
		record := make([]byte, kvpRecordBytes)
		copy(record, keyBuf)
		copy(record[kvpMaxKeyBytes:], valueBytes[start:end])
		records = append(records, record)
	}
	return records
}

// bootTimeFromProcStat returns the system boot time (unix seconds) from the /proc/stat btime line.
func bootTimeFromProcStat() (int64, error) {
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", procStatPath, err)
	}
	return parseBootTime(string(data))
}

// parseBootTime extracts the btime (boot time, unix seconds) value from /proc/stat contents.
func parseBootTime(procStat string) (int64, error) {
	for _, line := range strings.Split(procStat, "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			btime, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing btime from %s: %w", procStatPath, err)
			}
			return btime, nil
		}
	}
	return 0, fmt.Errorf("btime not found in %s", procStatPath)
}
