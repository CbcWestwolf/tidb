// Copyright 2023 PingCAP, Inc.
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
	"encoding/hex"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/docker/go-units"
	"github.com/jfcg/sorty/v2"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/lightning/common"
	"github.com/pingcap/tidb/pkg/lightning/membuf"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

// during test on ks3, we found that we can open about 8000 connections to ks3,
// bigger than that, we might receive "connection reset by peer" error, and
// the read speed will be very slow, still investigating the reason.
// Also open too many connections will take many memory in kernel, and the
// test is based on k8s pod, not sure how it will behave on EC2.
// but, ks3 supporter says there's no such limit on connections.
// And our target for global sort is AWS s3, this default value might not fit well.
// TODO: adjust it according to cloud storage.
const maxCloudStorageConnections = 1000

type memKVsAndBuffers struct {
	mu     sync.Mutex
	keys   [][]byte
	values [][]byte
	// memKVBuffers contains two types of buffer, first half are used for small block
	// buffer, second half are used for large one.
	memKVBuffers []*membuf.Buffer
	size         int
	droppedSize  int

	// temporary fields to store KVs to reduce slice allocations.
	keysPerFile        [][][]byte
	valuesPerFile      [][][]byte
	droppedSizePerFile []int
}

func (b *memKVsAndBuffers) build(ctx context.Context) {
	sumKVCnt := 0
	for _, keys := range b.keysPerFile {
		sumKVCnt += len(keys)
	}
	b.droppedSize = 0
	for _, size := range b.droppedSizePerFile {
		b.droppedSize += size
	}
	b.droppedSizePerFile = nil

	logutil.Logger(ctx).Info("building memKVsAndBuffers",
		zap.Int("sumKVCnt", sumKVCnt),
		zap.Int("droppedSize", b.droppedSize))

	b.keys = make([][]byte, 0, sumKVCnt)
	b.values = make([][]byte, 0, sumKVCnt)
	for i := range b.keysPerFile {
		b.keys = append(b.keys, b.keysPerFile[i]...)
		b.keysPerFile[i] = nil
		b.values = append(b.values, b.valuesPerFile[i]...)
		b.valuesPerFile[i] = nil
	}
	b.keysPerFile = nil
	b.valuesPerFile = nil
}

// Engine stored sorted key/value pairs in an external storage.
type Engine struct {
	storage           storage.ExternalStorage
	dataFiles         []string
	statsFiles        []string
	startKey          []byte
	endKey            []byte
	jobKeys           [][]byte
	splitKeys         [][]byte
	smallBlockBufPool *membuf.Pool
	largeBlockBufPool *membuf.Pool

	memKVsAndBuffers memKVsAndBuffers

	enableLocalStoreForCloud bool

	keyAdapter         common.KeyAdapter
	duplicateDetection bool
	duplicateDB        *pebble.DB
	dupDetectOpt       common.DupDetectOpt
	workerConcurrency  int
	ts                 uint64

	totalKVSize  int64
	totalKVCount int64

	importedKVSize  *atomic.Int64
	importedKVCount *atomic.Int64

	// For utilizing local disk
	db atomic.Pointer[pebble.DB]
}

var _ common.Engine = (*Engine)(nil)

const (
	memLimit       = 12 * units.GiB
	smallBlockSize = units.MiB
)

// NewExternalEngine creates an (external) engine.
func NewExternalEngine(
	storage storage.ExternalStorage,
	dataFiles []string,
	statsFiles []string,
	startKey []byte,
	endKey []byte,
	jobKeys [][]byte,
	splitKeys [][]byte,
	keyAdapter common.KeyAdapter,
	duplicateDetection bool,
	duplicateDB *pebble.DB,
	dupDetectOpt common.DupDetectOpt,
	workerConcurrency int,
	ts uint64,
	totalKVSize int64,
	totalKVCount int64,
	enableLocalStoreForCloud bool,
) common.Engine {
	memLimiter := membuf.NewLimiter(memLimit)
	return &Engine{
		storage:    storage,
		dataFiles:  dataFiles,
		statsFiles: statsFiles,
		startKey:   startKey,
		endKey:     endKey,
		jobKeys:    jobKeys,
		splitKeys:  splitKeys,
		smallBlockBufPool: membuf.NewPool(
			membuf.WithBlockNum(0),
			membuf.WithPoolMemoryLimiter(memLimiter),
			membuf.WithBlockSize(smallBlockSize),
		),
		largeBlockBufPool: membuf.NewPool(
			membuf.WithBlockNum(0),
			membuf.WithPoolMemoryLimiter(memLimiter),
			membuf.WithBlockSize(ConcurrentReaderBufferSizePerConc),
		),
		keyAdapter:               keyAdapter,
		duplicateDetection:       duplicateDetection,
		duplicateDB:              duplicateDB,
		dupDetectOpt:             dupDetectOpt,
		workerConcurrency:        workerConcurrency,
		ts:                       ts,
		totalKVSize:              totalKVSize,
		totalKVCount:             totalKVCount,
		importedKVSize:           atomic.NewInt64(0),
		importedKVCount:          atomic.NewInt64(0),
		enableLocalStoreForCloud: enableLocalStoreForCloud,
	}
}

func split[T any](in []T, groupNum int) [][]T {
	if len(in) == 0 {
		return nil
	}
	if groupNum <= 0 {
		groupNum = 1
	}
	ceil := (len(in) + groupNum - 1) / groupNum
	ret := make([][]T, 0, groupNum)
	l := len(in)
	for i := 0; i < l; i += ceil {
		if i+ceil > l {
			ret = append(ret, in[i:])
		} else {
			ret = append(ret, in[i:i+ceil])
		}
	}
	return ret
}

func getFilesReadConcurrency(
	ctx context.Context,
	storage storage.ExternalStorage,
	statsFiles []string,
	startKey, endKey []byte,
) ([]uint64, []uint64, error) {
	result := make([]uint64, len(statsFiles))
	offsets, err := seekPropsOffsets(ctx, []kv.Key{startKey, endKey}, statsFiles, storage)
	if err != nil {
		return nil, nil, err
	}
	startOffs, endOffs := offsets[0], offsets[1]
	for i := range statsFiles {
		expectedConc := (endOffs[i] - startOffs[i]) / uint64(ConcurrentReaderBufferSizePerConc)
		// let the stat internals cover the [startKey, endKey) since seekPropsOffsets
		// always return an offset that is less than or equal to the key.
		expectedConc += 1
		// readAllData will enable concurrent read and use large buffer if result[i] > 1
		// when expectedConc < readAllDataConcThreshold, we don't use concurrent read to
		// reduce overhead
		if expectedConc >= readAllDataConcThreshold {
			result[i] = expectedConc
		} else {
			result[i] = 1
		}
		// only log for files with expected concurrency > 1, to avoid too many logs
		if expectedConc > 1 {
			logutil.Logger(ctx).Info("found hotspot file in getFilesReadConcurrency",
				zap.String("filename", statsFiles[i]),
				zap.Uint64("startOffset", startOffs[i]),
				zap.Uint64("endOffset", endOffs[i]),
				zap.Uint64("expectedConc", expectedConc),
				zap.Uint64("concurrency", result[i]),
			)
		}
	}
	return result, startOffs, nil
}

func (e *Engine) loadBatchRegionData(ctx context.Context, jobKeys [][]byte, outCh chan<- common.DataAndRanges) error {
	readAndSortRateHist := metrics.GlobalSortReadFromCloudStorageRate.WithLabelValues("read_and_sort")
	readAndSortDurHist := metrics.GlobalSortReadFromCloudStorageDuration.WithLabelValues("read_and_sort")
	readRateHist := metrics.GlobalSortReadFromCloudStorageRate.WithLabelValues("read")
	readDurHist := metrics.GlobalSortReadFromCloudStorageDuration.WithLabelValues("read")
	sortRateHist := metrics.GlobalSortReadFromCloudStorageRate.WithLabelValues("sort")
	sortDurHist := metrics.GlobalSortReadFromCloudStorageDuration.WithLabelValues("sort")

	startKey := jobKeys[0]
	endKey := jobKeys[len(jobKeys)-1]
	readStart := time.Now()
	err := readAllData(
		ctx,
		e.storage,
		e.dataFiles,
		e.statsFiles,
		startKey,
		endKey,
		e.smallBlockBufPool,
		e.largeBlockBufPool,
		&e.memKVsAndBuffers,
	)
	if err != nil {
		return err
	}
	e.memKVsAndBuffers.build(ctx)

	readSecond := time.Since(readStart).Seconds()
	readDurHist.Observe(readSecond)
	logutil.Logger(ctx).Info("reading external storage in loadBatchRegionData",
		zap.Duration("cost time", time.Since(readStart)),
		zap.Int("droppedSize", e.memKVsAndBuffers.droppedSize))

	sortStart := time.Now()
	oldSortyGor := sorty.MaxGor
	sorty.MaxGor = uint64(e.workerConcurrency * 2)
	var dupKey atomic.Pointer[[]byte]
	sorty.Sort(len(e.memKVsAndBuffers.keys), func(i, k, r, s int) bool {
		cmp := bytes.Compare(e.memKVsAndBuffers.keys[i], e.memKVsAndBuffers.keys[k])
		if cmp < 0 { // strict comparator like < or >
			if r != s {
				e.memKVsAndBuffers.keys[r], e.memKVsAndBuffers.keys[s] = e.memKVsAndBuffers.keys[s], e.memKVsAndBuffers.keys[r]
				e.memKVsAndBuffers.values[r], e.memKVsAndBuffers.values[s] = e.memKVsAndBuffers.values[s], e.memKVsAndBuffers.values[r]
			}
			return true
		}
		if cmp == 0 && i != k {
			cloned := append([]byte(nil), e.memKVsAndBuffers.keys[i]...)
			dupKey.Store(&cloned)
		}
		return false
	})
	sorty.MaxGor = oldSortyGor
	sortSecond := time.Since(sortStart).Seconds()
	sortDurHist.Observe(sortSecond)
	logutil.Logger(ctx).Info("sorting in loadBatchRegionData",
		zap.Duration("cost time", time.Since(sortStart)))

	if k := dupKey.Load(); k != nil {
		return errors.Errorf("duplicate key found: %s", hex.EncodeToString(*k))
	}
	readAndSortSecond := time.Since(readStart).Seconds()
	readAndSortDurHist.Observe(readAndSortSecond)

	size := e.memKVsAndBuffers.size
	readAndSortRateHist.Observe(float64(size) / 1024.0 / 1024.0 / readAndSortSecond)
	readRateHist.Observe(float64(size) / 1024.0 / 1024.0 / readSecond)
	sortRateHist.Observe(float64(size) / 1024.0 / 1024.0 / sortSecond)

	data := &MemoryIngestData{
		keyAdapter:         e.keyAdapter,
		duplicateDetection: e.duplicateDetection,
		duplicateDB:        e.duplicateDB,
		dupDetectOpt:       e.dupDetectOpt,
		keys:               e.memKVsAndBuffers.keys,
		values:             e.memKVsAndBuffers.values,
		ts:                 e.ts,
		memBuf:             e.memKVsAndBuffers.memKVBuffers,
		refCnt:             atomic.NewInt64(0),
		importedKVSize:     e.importedKVSize,
		importedKVCount:    e.importedKVCount,
	}

	// release the reference of e.memKVsAndBuffers
	e.memKVsAndBuffers.keys = nil
	e.memKVsAndBuffers.values = nil
	e.memKVsAndBuffers.memKVBuffers = nil
	e.memKVsAndBuffers.size = 0

	ranges := make([]common.Range, 0, len(jobKeys)-1)
	prev, err2 := e.keyAdapter.Decode(nil, jobKeys[0])
	if err2 != nil {
		return err
	}
	for i := 1; i < len(jobKeys)-1; i++ {
		cur, err3 := e.keyAdapter.Decode(nil, jobKeys[i])
		if err3 != nil {
			return err3
		}
		ranges = append(ranges, common.Range{
			Start: prev,
			End:   cur,
		})
		prev = cur
	}
	// last range key may be a nextKey so we should try to remove the trailing 0 if decoding failed
	lastKey := jobKeys[len(jobKeys)-1]
	cur, err4 := e.tryDecodeEndKey(lastKey)
	if err4 != nil {
		return err4
	}
	ranges = append(ranges, common.Range{
		Start: prev,
		End:   cur,
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case outCh <- common.DataAndRanges{
		Data:         data,
		SortedRanges: ranges,
	}:
	}
	return nil
}

func (e *Engine) loadBatchRegionDataWithLocalStore(ctx context.Context, jobKeys [][]byte, outCh chan<- common.DataAndRanges) error {
	// TODO
	return nil
}

// LoadIngestData loads the data from the external storage to memory in [start,
// end) range, so local backend can ingest it. The used byte slice of ingest data
// are allocated from Engine.bufPool and must be released by
// MemoryIngestData.DecRef().
func (e *Engine) LoadIngestData(
	ctx context.Context,
	outCh chan<- common.DataAndRanges,
) error {
	// try to make every worker busy for each batch
	regionBatchSize := e.workerConcurrency
	failpoint.Inject("LoadIngestDataBatchSize", func(val failpoint.Value) {
		regionBatchSize = val.(int)
	})
	loadBatchRegionData := e.loadBatchRegionData
	if e.enableLocalStoreForCloud {
		loadBatchRegionData = e.loadBatchRegionDataWithLocalStore
	}
	for start := 0; start < len(e.jobKeys)-1; start += regionBatchSize {
		// want to generate N ranges, so we need N+1 keys
		end := min(1+start+regionBatchSize, len(e.jobKeys))
		err := loadBatchRegionData(ctx, e.jobKeys[start:end], outCh)
		if err != nil {
			return err
		}
	}
	return nil
}

// KVStatistics returns the total kv size and total kv count.
func (e *Engine) KVStatistics() (totalKVSize int64, totalKVCount int64) {
	return e.totalKVSize, e.totalKVCount
}

// ImportedStatistics returns the imported kv size and imported kv count.
func (e *Engine) ImportedStatistics() (importedSize int64, importedKVCount int64) {
	return e.importedKVSize.Load(), e.importedKVCount.Load()
}

// ID is the identifier of an engine.
func (e *Engine) ID() string {
	return "external"
}

// GetKeyRange implements common.Engine.
func (e *Engine) GetKeyRange() (startKey []byte, endKey []byte, err error) {
	if _, ok := e.keyAdapter.(common.NoopKeyAdapter); ok {
		return e.startKey, e.endKey, nil
	}

	startKey, err = e.keyAdapter.Decode(nil, e.startKey)
	if err != nil {
		return nil, nil, err
	}
	endKey, err = e.tryDecodeEndKey(e.endKey)
	if err != nil {
		return nil, nil, err
	}
	return startKey, endKey, nil
}

// GetRegionSplitKeys implements common.Engine.
func (e *Engine) GetRegionSplitKeys() ([][]byte, error) {
	splitKeys := make([][]byte, len(e.splitKeys))
	var (
		err      error
		splitKey []byte
	)
	for i, k := range e.splitKeys {
		if i < len(e.splitKeys)-1 {
			splitKey, err = e.keyAdapter.Decode(nil, k)
		} else {
			splitKey, err = e.tryDecodeEndKey(k)
		}
		if err != nil {
			return nil, err
		}
		splitKeys[i] = splitKey
	}
	return splitKeys, nil
}

// tryDecodeEndKey tries to decode the key from two sources.
// When duplicate detection feature is enabled, the **end key** comes from
// DupDetectKeyAdapter.Encode or Key.Next(). We try to decode it and check the
// error.
func (e Engine) tryDecodeEndKey(key []byte) (decoded []byte, err error) {
	decoded, err = e.keyAdapter.Decode(nil, key)
	if err == nil {
		return
	}
	if _, ok := e.keyAdapter.(common.NoopKeyAdapter); ok {
		// NoopKeyAdapter.Decode always return nil error
		intest.Assert(false, "Unreachable code path")
		return nil, err
	}
	// handle the case that end key is from Key.Next()
	if key[len(key)-1] != 0 {
		return nil, err
	}
	key = key[:len(key)-1]
	decoded, err = e.keyAdapter.Decode(nil, key)
	if err != nil {
		return nil, err
	}
	return kv.Key(decoded).Next(), nil
}

// Close implements common.Engine.
func (e *Engine) Close() error {
	if e.smallBlockBufPool != nil {
		e.smallBlockBufPool.Destroy()
		e.smallBlockBufPool = nil
	}
	if e.largeBlockBufPool != nil {
		e.largeBlockBufPool.Destroy()
		e.largeBlockBufPool = nil
	}
	e.storage.Close()
	return nil
}

// Reset resets the memory buffer pool.
func (e *Engine) Reset() error {
	memLimiter := membuf.NewLimiter(memLimit)
	if e.smallBlockBufPool != nil {
		e.smallBlockBufPool.Destroy()
		e.smallBlockBufPool = membuf.NewPool(
			membuf.WithBlockNum(0),
			membuf.WithPoolMemoryLimiter(memLimiter),
			membuf.WithBlockSize(smallBlockSize),
		)
	}
	if e.largeBlockBufPool != nil {
		e.largeBlockBufPool.Destroy()
		e.largeBlockBufPool = membuf.NewPool(
			membuf.WithBlockNum(0),
			membuf.WithPoolMemoryLimiter(memLimiter),
			membuf.WithBlockSize(ConcurrentReaderBufferSizePerConc),
		)
	}
	return nil
}
