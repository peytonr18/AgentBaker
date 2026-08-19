package provisioning

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Hyper-V KVP on-disk wire format. Keys and values are stored as zero-padded UTF-8 in
// fixed-width records, matching the Linux kernel HV_KVP_EXCHANGE_MAX_* sizes that
// hv_kvp_daemon exchanges. Mirrors azure-init (libazureinit/src/kvp.rs).
const (
	// kvpMaxKeyBytes is the fixed key field width (HV_KVP_EXCHANGE_MAX_KEY_SIZE).
	kvpMaxKeyBytes = 512
	// kvpMaxValueBytes is the fixed value field width (HV_KVP_EXCHANGE_MAX_VALUE_SIZE).
	kvpMaxValueBytes = 2048
	// kvpChunkBytes is the maximum host-readable value size (HV_KVP_AZURE_MAX_VALUE_SIZE).
	// Two bytes are reserved for the kernel's string terminator handling.
	kvpChunkBytes = 1022
	// kvpRecordBytes is the total fixed record size on disk.
	kvpRecordBytes = kvpMaxKeyBytes + kvpMaxValueBytes
	// kvpProvisioningReportKey is the key the Azure/Hyper-V host parses for the health report.
	kvpProvisioningReportKey = "PROVISIONING_REPORT"
	// kvpDmesgKey is the key under which recent kernel log output is stored for diagnostics.
	kvpDmesgKey = "dmesg"
	// kvpChunkKeySeparator joins a key with a chunk index when a value is too large for one
	// host-readable record, producing "PROVISIONING_REPORT|0", "PROVISIONING_REPORT|1", ... A
	// reader reassembles the value by concatenating the chunks in ascending index order.
	kvpChunkKeySeparator = "|"

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

// KVPWriter writes provisioning telemetry to the Hyper-V guest KVP pool file so the
// Azure/Hyper-V host can read it. Records use the fixed-width 512+2048 wire format. Writes use
// upsert semantics and hold both flock and fcntl locks so repeated reports replace the previous
// outcome and coordinate with both cloud-init/azure-init and hv_kvp_daemon.
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

// WriteProvisioningReport truncates stale pool data, upserts recent dmesg output for diagnostics
// (best-effort), then upserts the encoded provisioning report. dmesg failures do not prevent the
// report from being written.
func (w *KVPWriter) WriteProvisioningReport(report string) error {
	entries := []kvpEntry{{key: kvpProvisioningReportKey, value: report}}
	if dmesg, err := captureDmesg(); err != nil {
		slog.Warn("failed to write dmesg to KVP (continuing)", "error", err)
	} else if dmesg != "" {
		entries = append([]kvpEntry{{key: kvpDmesgKey, value: dmesg}}, entries...)
	}
	return w.upsert(entries)
}

type kvpEntry struct {
	key   string
	value string
}

// captureDmesg captures the newest host-readable tail of dmesg output.
func captureDmesg() (string, error) {
	out, err := lookupDmesg()
	if err != nil {
		return "", fmt.Errorf("running dmesg: %w", err)
	}
	value := strings.ToValidUTF8(string(out), "\uFFFD")
	if len(value) <= kvpChunkBytes {
		return value, nil
	}
	start := len(value) - kvpChunkBytes
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:], nil
}

// upsert rewrites the pool so each entry's key holds exactly the supplied value, replacing any
// previous records for that key (including the "key|<index>" chunk records of an earlier oversized
// value) while preserving records owned by other writers.
func (w *KVPWriter) upsert(entries []kvpEntry) (retErr error) {
	encoded := make([][]byte, 0, len(entries))
	rewritten := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, duplicate := rewritten[entry.key]; duplicate {
			return fmt.Errorf("duplicate KVP update key %q", entry.key)
		}
		records, err := encodeKVPEntry(entry.key, entry.value)
		if err != nil {
			return err
		}
		encoded = append(encoded, records...)
		rewritten[entry.key] = struct{}{}
	}

	// #nosec G304 -- the pool path is an operator/config value, not untrusted input.
	f, err := os.OpenFile(w.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening KVP pool file %s: %w", w.Path, err)
	}
	defer func() { _ = f.Close() }()

	if err := lockKVPFile(f); err != nil {
		return fmt.Errorf("locking KVP pool file: %w", err)
	}
	defer func() {
		if err := unlockKVPFile(f); retErr == nil && err != nil {
			retErr = fmt.Errorf("unlocking KVP pool file: %w", err)
		}
	}()

	if err := w.truncateIfStale(f); err != nil {
		slog.Warn("failed to truncate stale KVP pool file", "path", w.Path, "error", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking KVP pool file: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("reading KVP pool file: %w", err)
	}
	if len(data)%kvpRecordBytes != 0 {
		return fmt.Errorf("KVP pool file size %d is not a multiple of record size %d", len(data), kvpRecordBytes)
	}

	updated := make([]byte, 0, len(data)+len(encoded)*kvpRecordBytes)
	for offset := 0; offset < len(data); offset += kvpRecordBytes {
		key := strings.TrimRight(string(data[offset:offset+kvpMaxKeyBytes]), "\x00")
		if _, replace := rewritten[baseKeyOf(key)]; !replace {
			updated = append(updated, data[offset:offset+kvpRecordBytes]...)
		}
	}
	for _, record := range encoded {
		updated = append(updated, record...)
	}

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating KVP pool file: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding KVP pool file: %w", err)
	}
	if _, err := f.Write(updated); err != nil {
		return fmt.Errorf("writing KVP pool file: %w", err)
	}
	return nil
}

// truncateIfStale clears the locked pool file if its mtime predates the current boot.
func (w *KVPWriter) truncateIfStale(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat KVP pool file: %w", err)
	}
	bootTime, err := w.bootTime()
	if err != nil {
		return fmt.Errorf("determining boot time: %w", err)
	}
	if info.ModTime().Unix() < bootTime {
		if err := f.Truncate(0); err != nil {
			return fmt.Errorf("truncating stale KVP pool file: %w", err)
		}
	}
	return nil
}

// encodeKVPRecord encodes one host-readable key/value pair into a fixed-width record.
func encodeKVPRecord(key, value string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("KVP key must not be empty")
	}
	if strings.IndexByte(key, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
		return nil, errors.New("KVP key and value must not contain null bytes")
	}
	if len(key) > kvpMaxKeyBytes {
		return nil, fmt.Errorf("KVP key is %d bytes; maximum is %d", len(key), kvpMaxKeyBytes)
	}
	if len(value) > kvpChunkBytes {
		return nil, fmt.Errorf("KVP value is %d bytes; host-readable maximum is %d", len(value), kvpChunkBytes)
	}

	record := make([]byte, kvpRecordBytes)
	copy(record, key)
	copy(record[kvpMaxKeyBytes:], value)
	return record, nil
}

// encodeKVPEntry encodes one logical key/value into one or more fixed-width records. A value that
// fits one host-readable record (<= kvpChunkBytes) is stored verbatim under key, so a short report
// stays byte-for-byte identical to a plain single record. A larger value is split on UTF-8
// boundaries into chunks stored under indexed keys "key|0", "key|1", ...; a reader reassembles it
// by concatenating the chunks in ascending index order. This is cloud-init's key-suffix chunk
// indexing without cloud-init's JSON value envelope.
func encodeKVPEntry(key, value string) ([][]byte, error) {
	if len(value) <= kvpChunkBytes {
		record, err := encodeKVPRecord(key, value)
		if err != nil {
			return nil, err
		}
		return [][]byte{record}, nil
	}
	chunks := splitOnRuneBoundary(value, kvpChunkBytes)
	records := make([][]byte, 0, len(chunks))
	for i, chunk := range chunks {
		record, err := encodeKVPRecord(chunkKey(key, i), chunk)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// chunkKey builds the indexed key "base|index" for chunk index of an oversized value.
func chunkKey(base string, index int) string {
	return base + kvpChunkKeySeparator + strconv.Itoa(index)
}

// baseKeyOf returns the logical key a record key belongs to: the key itself, or the base of a
// "base|<index>" chunk key. It lets upsert replace every prior record for a key regardless of
// whether the previous value was stored as one record or as indexed chunks.
func baseKeyOf(recordKey string) string {
	sep := strings.LastIndex(recordKey, kvpChunkKeySeparator)
	if sep < 0 {
		return recordKey
	}
	if isAllDigits(recordKey[sep+len(kvpChunkKeySeparator):]) {
		return recordKey[:sep]
	}
	return recordKey
}

// splitOnRuneBoundary splits s into consecutive pieces of at most maxBytes bytes without cutting a
// UTF-8 rune, so each piece is independently valid UTF-8 and concatenating the pieces restores s.
func splitOnRuneBoundary(s string, maxBytes int) []string {
	var chunks []string
	for len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 { // a single rune wider than maxBytes; fall back to a byte split.
			cut = maxBytes
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
	}
	return append(chunks, s)
}

// isAllDigits reports whether s is non-empty and every byte is an ASCII digit.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func lockKVPFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(io.SeekStart)}
	if err := unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLKW, &lock); err != nil {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return err
	}
	return nil
}

func unlockKVPFile(f *os.File) error {
	lock := unix.Flock_t{Type: unix.F_UNLCK, Whence: int16(io.SeekStart)}
	fcntlErr := unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lock)
	flockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return errors.Join(fcntlErr, flockErr)
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
