package provisioningtest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Hyper-V KVP on-disk record widths, mirroring the wire format written by the provisioning
// package (fixed 512-byte key + 2048-byte value, zero-padded).
const (
	kvpKeyBytes    = 512
	kvpValueBytes  = 2048
	kvpRecordBytes = kvpKeyBytes + kvpValueBytes
)

// KVPRecord is a decoded Hyper-V KVP pool record.
type KVPRecord struct {
	Key   string
	Value string
}

// ReadKVPPool reads a Hyper-V KVP pool file and decodes its fixed-width records, trimming zero
// padding from keys and values.
func ReadKVPPool(path string) ([]KVPRecord, error) {
	// #nosec G304 -- decodes a caller-provided KVP pool path in tests.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data)%kvpRecordBytes != 0 {
		return nil, fmt.Errorf("KVP pool size %d is not a multiple of record size %d", len(data), kvpRecordBytes)
	}
	var records []KVPRecord
	for off := 0; off+kvpRecordBytes <= len(data); off += kvpRecordBytes {
		key := strings.TrimRight(string(data[off:off+kvpKeyBytes]), "\x00")
		value := strings.TrimRight(string(data[off+kvpKeyBytes:off+kvpRecordBytes]), "\x00")
		records = append(records, KVPRecord{Key: key, Value: value})
	}
	return records, nil
}

// FindKVPValue returns the unique value for key. Duplicate keys are rejected because
// hv_kvp_daemon does not provide ordered chunk reassembly.
func FindKVPValue(records []KVPRecord, key string) (string, bool, error) {
	value, found := "", false
	for _, record := range records {
		if record.Key == key {
			if found {
				return "", false, fmt.Errorf("KVP pool contains duplicate key %q", key)
			}
			value, found = record.Value, true
		}
	}
	return value, found, nil
}

// ReassembleKVPValue returns the full value stored under key. A value written verbatim under key is
// returned as-is; an oversized value split across "key|0", "key|1", ... chunk records is
// reassembled by concatenating the chunks in ascending index order, mirroring how a host consumer
// reconstructs it.
func ReassembleKVPValue(records []KVPRecord, key string) (string, bool, error) {
	if value, found, err := FindKVPValue(records, key); err != nil || found {
		return value, found, err
	}
	chunks := map[int]string{}
	for _, record := range records {
		suffix, ok := strings.CutPrefix(record.Key, key+"|")
		if !ok {
			continue
		}
		index, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if _, exists := chunks[index]; exists {
			return "", false, fmt.Errorf("duplicate KVP chunk %q", record.Key)
		}
		chunks[index] = record.Value
	}
	if len(chunks) == 0 {
		return "", false, nil
	}
	var b strings.Builder
	for i := 0; i < len(chunks); i++ {
		chunk, ok := chunks[i]
		if !ok {
			return "", false, fmt.Errorf("missing KVP chunk %s|%d", key, i)
		}
		b.WriteString(chunk)
	}
	return b.String(), true, nil
}
