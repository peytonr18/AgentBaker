package provisioningtest

import (
	"os"
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
	var records []KVPRecord
	for off := 0; off+kvpRecordBytes <= len(data); off += kvpRecordBytes {
		key := strings.TrimRight(string(data[off:off+kvpKeyBytes]), "\x00")
		value := strings.TrimRight(string(data[off+kvpKeyBytes:off+kvpRecordBytes]), "\x00")
		records = append(records, KVPRecord{Key: key, Value: value})
	}
	return records, nil
}

// FindKVPValue returns the value of the last record with the given key, and whether it was found.
func FindKVPValue(records []KVPRecord, key string) (string, bool) {
	value, found := "", false
	for _, record := range records {
		if record.Key == key {
			value, found = record.Value, true
		}
	}
	return value, found
}
