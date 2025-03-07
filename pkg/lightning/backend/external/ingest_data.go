// Copyright 2025 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package external

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/cockroachdb/pebble"
	"github.com/pingcap/tidb/pkg/lightning/common"
	"github.com/pingcap/tidb/pkg/lightning/log"
	"github.com/pingcap/tidb/pkg/lightning/membuf"
	"go.uber.org/atomic"
)

const maxPebbleBatchSize = 4 << 20

var _ common.IngestData = (*MemoryIngestData)(nil)
var _ common.IngestData = (*PebbleIngestData)(nil)

// MemoryIngestData is the in-memory implementation of IngestData.
type MemoryIngestData struct {
	keyAdapter         common.KeyAdapter
	duplicateDetection bool
	duplicateDB        *pebble.DB
	dupDetectOpt       common.DupDetectOpt

	keys   [][]byte
	values [][]byte
	ts     uint64

	memBuf          []*membuf.Buffer
	refCnt          *atomic.Int64
	importedKVSize  *atomic.Int64
	importedKVCount *atomic.Int64
}

func (m *MemoryIngestData) firstAndLastKeyIndex(lowerBound, upperBound []byte) (int, int) {
	firstKeyIdx := 0
	if len(lowerBound) > 0 {
		lowerBound = m.keyAdapter.Encode(nil, lowerBound, common.MinRowID)
		firstKeyIdx = sort.Search(len(m.keys), func(i int) bool {
			return bytes.Compare(lowerBound, m.keys[i]) <= 0
		})
		if firstKeyIdx == len(m.keys) {
			return -1, -1
		}
	}

	lastKeyIdx := len(m.keys) - 1
	if len(upperBound) > 0 {
		upperBound = m.keyAdapter.Encode(nil, upperBound, common.MinRowID)
		i := sort.Search(len(m.keys), func(i int) bool {
			reverseIdx := len(m.keys) - 1 - i
			return bytes.Compare(upperBound, m.keys[reverseIdx]) > 0
		})
		if i == len(m.keys) {
			// should not happen
			return -1, -1
		}
		lastKeyIdx = len(m.keys) - 1 - i
	}
	return firstKeyIdx, lastKeyIdx
}

// GetFirstAndLastKey implements IngestData.GetFirstAndLastKey.
func (m *MemoryIngestData) GetFirstAndLastKey(lowerBound, upperBound []byte) ([]byte, []byte, error) {
	firstKeyIdx, lastKeyIdx := m.firstAndLastKeyIndex(lowerBound, upperBound)
	if firstKeyIdx < 0 || firstKeyIdx > lastKeyIdx {
		return nil, nil, nil
	}
	firstKey, err := m.keyAdapter.Decode(nil, m.keys[firstKeyIdx])
	if err != nil {
		return nil, nil, err
	}
	lastKey, err := m.keyAdapter.Decode(nil, m.keys[lastKeyIdx])
	if err != nil {
		return nil, nil, err
	}
	return firstKey, lastKey, nil
}

type memoryDataIter struct {
	keys   [][]byte
	values [][]byte

	firstKeyIdx int
	lastKeyIdx  int
	curIdx      int
}

// First implements ForwardIter.
func (m *memoryDataIter) First() bool {
	if m.firstKeyIdx < 0 {
		return false
	}
	m.curIdx = m.firstKeyIdx
	return true
}

// Valid implements ForwardIter.
func (m *memoryDataIter) Valid() bool {
	return m.firstKeyIdx <= m.curIdx && m.curIdx <= m.lastKeyIdx
}

// Next implements ForwardIter.
func (m *memoryDataIter) Next() bool {
	m.curIdx++
	return m.Valid()
}

// Key implements ForwardIter.
func (m *memoryDataIter) Key() []byte {
	return m.keys[m.curIdx]
}

// Value implements ForwardIter.
func (m *memoryDataIter) Value() []byte {
	return m.values[m.curIdx]
}

// Close implements ForwardIter.
func (m *memoryDataIter) Close() error {
	return nil
}

// Error implements ForwardIter.
func (m *memoryDataIter) Error() error {
	return nil
}

// ReleaseBuf implements ForwardIter.
func (m *memoryDataIter) ReleaseBuf() {}

type memoryDataDupDetectIter struct {
	iter           *memoryDataIter
	dupDetector    *common.DupDetector
	err            error
	curKey, curVal []byte
	buf            *membuf.Buffer
}

// First implements ForwardIter.
func (m *memoryDataDupDetectIter) First() bool {
	if m.err != nil || !m.iter.First() {
		return false
	}
	m.curKey, m.curVal, m.err = m.dupDetector.Init(m.iter)
	return m.Valid()
}

// Valid implements ForwardIter.
func (m *memoryDataDupDetectIter) Valid() bool {
	return m.err == nil && m.iter.Valid()
}

// Next implements ForwardIter.
func (m *memoryDataDupDetectIter) Next() bool {
	if m.err != nil {
		return false
	}
	key, val, ok, err := m.dupDetector.Next(m.iter)
	if err != nil {
		m.err = err
		return false
	}
	if !ok {
		return false
	}
	m.curKey, m.curVal = key, val
	return true
}

// Key implements ForwardIter.
func (m *memoryDataDupDetectIter) Key() []byte {
	return m.buf.AddBytes(m.curKey)
}

// Value implements ForwardIter.
func (m *memoryDataDupDetectIter) Value() []byte {
	return m.buf.AddBytes(m.curVal)
}

// Close implements ForwardIter.
func (m *memoryDataDupDetectIter) Close() error {
	m.buf.Destroy()
	return m.dupDetector.Close()
}

// Error implements ForwardIter.
func (m *memoryDataDupDetectIter) Error() error {
	return m.err
}

// ReleaseBuf implements ForwardIter.
func (m *memoryDataDupDetectIter) ReleaseBuf() {
	m.buf.Reset()
}

// NewIter implements IngestData.NewIter.
func (m *MemoryIngestData) NewIter(
	ctx context.Context,
	lowerBound, upperBound []byte,
	bufPool *membuf.Pool,
) common.ForwardIter {
	firstKeyIdx, lastKeyIdx := m.firstAndLastKeyIndex(lowerBound, upperBound)
	iter := &memoryDataIter{
		keys:        m.keys,
		values:      m.values,
		firstKeyIdx: firstKeyIdx,
		lastKeyIdx:  lastKeyIdx,
	}
	if !m.duplicateDetection {
		return iter
	}
	logger := log.FromContext(ctx)
	detector := common.NewDupDetector(m.keyAdapter, m.duplicateDB.NewBatch(), logger, m.dupDetectOpt)
	return &memoryDataDupDetectIter{
		iter:        iter,
		dupDetector: detector,
		buf:         bufPool.NewBuffer(),
	}
}

// GetTS implements IngestData.GetTS.
func (m *MemoryIngestData) GetTS() uint64 {
	return m.ts
}

// IncRef implements IngestData.IncRef.
func (m *MemoryIngestData) IncRef() {
	m.refCnt.Inc()
}

// DecRef implements IngestData.DecRef.
func (m *MemoryIngestData) DecRef() {
	if m.refCnt.Dec() == 0 {
		m.keys = nil
		m.values = nil
		for _, b := range m.memBuf {
			b.Destroy()
		}
	}
}

// Finish implements IngestData.Finish.
func (m *MemoryIngestData) Finish(totalBytes, totalCount int64) {
	m.importedKVSize.Add(totalBytes)
	m.importedKVCount.Add(totalCount)
}

// PebbleIngestData is an implementation of IngestData utilizing pebble.
// Compared with MemoryIngestData, it costs less memory.
type PebbleIngestData struct {
	db    *pebble.DB
	batch *pebble.Batch

	// duplicate detection
	keyAdapter         common.KeyAdapter
	duplicateDetection bool
	duplicateDB        *pebble.DB
	dupDetectOpt       common.DupDetectOpt

	ts uint64

	refCnt          *atomic.Int64
	importedKVSize  *atomic.Int64
	importedKVCount *atomic.Int64
}

func (PebbleIngestData) GetFirstAndLastKey(lowerBound, upperBound []byte) ([]byte, []byte, error) {
	// TODO
	return nil, nil, nil
}

func (p *PebbleIngestData) NewIter(ctx context.Context, lowerBound, upperBound []byte, bufPool *membuf.Pool) common.ForwardIter {
	return nil
}

func (p PebbleIngestData) GetTS() uint64 {
	return p.ts
}

func (p *PebbleIngestData) IncRef() {
	p.refCnt.Inc()
}

func (p *PebbleIngestData) DecRef() {
	if p.refCnt.Dec() == 0 {
		// TODO: clean
	}
}

func (p *PebbleIngestData) Finish(totalBytes, totalCount int64) {
	p.importedKVSize.Add(totalBytes)
	p.importedKVCount.Add(totalCount)
}

// diskIter is a disk-based implementation of ForwardIter
// to avoid OOM when dealing with large datasets.
type diskIter struct {
	tempFile     *os.File
	currentKey   []byte
	currentValue []byte
	buf          *membuf.Buffer

	// Error state
	err error

	// File navigation
	valid        bool
	keyLengthBuf [8]byte
	valLengthBuf [8]byte

	// Bounds
	lowerBound []byte
	upperBound []byte

	// For cursor position and range validation
	reachedEnd bool
}

// NewDiskIter creates a new disk-based iterator using a temporary file.
func NewDiskIter(ctx context.Context, kvs map[string][]byte, lowerBound, upperBound []byte, bufPool *membuf.Pool) (*diskIter, error) {
	// Create a temporary directory for our iterator
	tempDir := os.TempDir()
	tempFile, err := os.CreateTemp(tempDir, "diskiter-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	// Create and initialize the disk iterator
	iter := &diskIter{
		tempFile:   tempFile,
		buf:        bufPool.NewBuffer(),
		lowerBound: lowerBound,
		upperBound: upperBound,
	}

	// Sort all keys for ordered iteration
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Write key-value pairs to temporary file
	// Format: [key length (8 bytes)][key bytes][value length (8 bytes)][value bytes]
	for _, key := range keys {
		// Skip keys outside our bounds
		if (len(lowerBound) > 0 && bytes.Compare([]byte(key), lowerBound) < 0) ||
			(len(upperBound) > 0 && bytes.Compare([]byte(key), upperBound) >= 0) {
			continue
		}

		value := kvs[key]
		keyLen := int64(len(key))
		valueLen := int64(len(value))

		// Write key length
		binary.BigEndian.PutUint64(iter.keyLengthBuf[:], uint64(keyLen))
		if _, err := tempFile.Write(iter.keyLengthBuf[:]); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("failed to write key length: %w", err)
		}

		// Write key
		if _, err := tempFile.Write([]byte(key)); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("failed to write key: %w", err)
		}

		// Write value length
		binary.BigEndian.PutUint64(iter.valLengthBuf[:], uint64(valueLen))
		if _, err := tempFile.Write(iter.valLengthBuf[:]); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("failed to write value length: %w", err)
		}

		// Write value
		if _, err := tempFile.Write(value); err != nil {
			tempFile.Close()
			os.Remove(tempFile.Name())
			return nil, fmt.Errorf("failed to write value: %w", err)
		}
	}

	// Reset file pointer to beginning for iteration
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		tempFile.Close()
		os.Remove(tempFile.Name())
		return nil, fmt.Errorf("failed to reset file cursor: %w", err)
	}

	return iter, nil
}

// First implements ForwardIter.First
func (d *diskIter) First() bool {
	if d.err != nil {
		return false
	}

	// Reset file cursor to beginning
	if _, err := d.tempFile.Seek(0, io.SeekStart); err != nil {
		d.err = fmt.Errorf("failed to reset file cursor: %w", err)
		return false
	}

	// Read first entry
	return d.readNext()
}

// readNext reads the next key-value pair from disk
func (d *diskIter) readNext() bool {
	// If we've already reached the end or have an error, return false
	if d.reachedEnd || d.err != nil {
		d.valid = false
		return false
	}

	// Read key length
	n, err := io.ReadFull(d.tempFile, d.keyLengthBuf[:])
	if err == io.EOF || n == 0 {
		d.reachedEnd = true
		d.valid = false
		return false
	}
	if err != nil {
		d.err = fmt.Errorf("failed to read key length: %w", err)
		d.valid = false
		return false
	}

	keyLen := binary.BigEndian.Uint64(d.keyLengthBuf[:])

	// Read key
	d.currentKey = make([]byte, keyLen)
	if _, err := io.ReadFull(d.tempFile, d.currentKey); err != nil {
		d.err = fmt.Errorf("failed to read key: %w", err)
		d.valid = false
		return false
	}

	// Check if key is within bounds
	if (len(d.lowerBound) > 0 && bytes.Compare(d.currentKey, d.lowerBound) < 0) ||
		(len(d.upperBound) > 0 && bytes.Compare(d.currentKey, d.upperBound) >= 0) {
		// Skip this entry and try the next one
		return d.readNext()
	}

	// Read value length
	if _, err := io.ReadFull(d.tempFile, d.valLengthBuf[:]); err != nil {
		d.err = fmt.Errorf("failed to read value length: %w", err)
		d.valid = false
		return false
	}

	valueLen := binary.BigEndian.Uint64(d.valLengthBuf[:])

	// Read value
	d.currentValue = make([]byte, valueLen)
	if _, err := io.ReadFull(d.tempFile, d.currentValue); err != nil {
		d.err = fmt.Errorf("failed to read value: %w", err)
		d.valid = false
		return false
	}

	d.valid = true
	return true
}

// Valid implements ForwardIter.Valid
func (d *diskIter) Valid() bool {
	return d.valid && d.err == nil
}

// Next implements ForwardIter.Next
func (d *diskIter) Next() bool {
	if !d.valid || d.err != nil {
		return false
	}

	return d.readNext()
}

// Key implements ForwardIter.Key
func (d *diskIter) Key() []byte {
	if !d.valid {
		return nil
	}
	return d.buf.AddBytes(d.currentKey)
}

// Value implements ForwardIter.Value
func (d *diskIter) Value() []byte {
	if !d.valid {
		return nil
	}
	return d.buf.AddBytes(d.currentValue)
}

// Close implements ForwardIter.Close
func (d *diskIter) Close() error {
	// Clean up resources
	if d.buf != nil {
		d.buf.Destroy()
	}

	tempFileName := ""
	if d.tempFile != nil {
		tempFileName = d.tempFile.Name()
		err := d.tempFile.Close()
		if err != nil && d.err == nil {
			d.err = err
		}
	}

	// Remove the temporary file
	if tempFileName != "" {
		if err := os.Remove(tempFileName); err != nil && d.err == nil {
			d.err = err
		}
	}

	return d.err
}

// Error implements ForwardIter.Error
func (d *diskIter) Error() error {
	return d.err
}

// ReleaseBuf implements ForwardIter.ReleaseBuf
func (d *diskIter) ReleaseBuf() {
	if d.buf != nil {
		d.buf.Reset()
	}
}
