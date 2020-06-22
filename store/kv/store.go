// Copyright 2020 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kv

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/dtracing"
	"github.com/dfuse-io/fluxdb/store"
	kv "github.com/dfuse-io/kvdb/store"
	"go.uber.org/zap"
)

var TblPrefixName = map[byte]string{
	TblPrefixRows:  "tablet",
	TblPrefixIndex: "index",
	TblPrefixLast:  "block",
}

const (
	TblPrefixRows  = 0x00
	TblPrefixIndex = 0x01
	TblPrefixLast  = 0x03
)

var TableMapper = map[byte]string{}

type KVStore struct {
	db kv.KVStore
}

func NewStore(dsnString string) (*KVStore, error) {
	store, err := kv.New(dsnString, kv.WithEmptyValueSupport)
	if err != nil {
		return nil, fmt.Errorf("cannot create new kv store: %w", err)
	}

	return &KVStore{
		db: store,
	}, nil

}

func (s *KVStore) Close() error {
	if closer, ok := s.db.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (s *KVStore) NewBatch(logger *zap.Logger) store.Batch {
	return newBatch(s, logger)
}

func (s *KVStore) FetchSingletEntry(ctx context.Context, keyStart, keyEnd string) (key string, value []byte, err error) {
	err = s.scanRange(ctx, TblPrefixRows, keyStart, keyEnd, 1, func(rowKey string, rowValue []byte) error {
		key = rowKey
		value = rowValue

		// We only ever check a single row
		return store.BreakScan
	})

	if err != nil && err != store.BreakScan {
		return "", nil, fmt.Errorf("unable to fetch single tablet row (range [%s, %s[): %w", keyStart, keyEnd, err)
	}

	return key, value, nil
}

func (s *KVStore) FetchIndex(ctx context.Context, tableKey, prefixKey, keyStart string) (rowKey string, rawIndex []byte, err error) {
	err = s.scanInfiniteRange(ctx, TblPrefixIndex, keyStart, 1, func(key string, value []byte) error {
		if !strings.HasPrefix(key, prefixKey) {
			return store.BreakScan
		}

		rowKey = key
		rawIndex = value

		// We always only check a single row
		return store.BreakScan
	})

	if err != nil && err != store.BreakScan {
		return "", nil, fmt.Errorf("unable to fetch index for key prefix %q: %w", prefixKey, err)
	}

	if rawIndex == nil {
		return "", nil, store.ErrNotFound
	}

	return rowKey, rawIndex, nil
}

func (s *KVStore) HasTabletRow(ctx context.Context, tabletKey string) (exists bool, err error) {
	err = s.scanPrefix(ctx, TblPrefixRows, tabletKey, 1, func(_ string, _ []byte) error {
		exists = true
		return store.BreakScan
	})

	if err != nil && err != store.BreakScan {
		return false, fmt.Errorf("scan prefix %q for table %q: %w", tabletKey, TblPrefixName[TblPrefixRows], err)
	}

	return exists, nil
}

func (s *KVStore) FetchTabletRow(ctx context.Context, key string) (value []byte, err error) {
	return s.fetchKey(ctx, TblPrefixRows, key)
}

func (s *KVStore) FetchTabletRows(ctx context.Context, keys []string, onTabletRow store.OnTabletRow) error {
	return s.fetchKeys(ctx, TblPrefixRows, keys, onTabletRow)
}

func (s *KVStore) ScanTabletRows(ctx context.Context, keyStart, keyEnd string, onTabletRow store.OnTabletRow) error {
	err := s.scanRange(ctx, TblPrefixRows, keyStart, keyEnd, kv.Unlimited, func(key string, value []byte) error {
		err := onTabletRow(key, value)
		if err == store.BreakScan {
			return store.BreakScan
		}

		if err != nil {
			return fmt.Errorf("on tablet row for key %q failed: %w", key, err)
		}

		return nil
	})

	if err != nil && err != store.BreakScan {
		return fmt.Errorf("unable to scan tablet rows [%q, %q[: %w", keyStart, keyEnd, err)
	}

	return nil
}

func (s *KVStore) FetchLastWrittenBlock(ctx context.Context, key string) (out bstream.BlockRef, err error) {
	zlog.Debug("fetching last written block", zap.String("key", key))
	value, err := s.fetchKey(ctx, TblPrefixLast, key)
	if err != nil {
		return nil, err
	}

	return bstream.BlockRefFromID(string(value)), nil
}

func (s *KVStore) ScanLastShardsWrittenBlock(ctx context.Context, keyPrefix string, onBlockRef store.OnBlockRef) error {
	err := s.scanPrefix(ctx, TblPrefixLast, keyPrefix, kv.Unlimited, func(key string, value []byte) error {
		err := onBlockRef(key, bstream.BlockRefFromID(value))
		if err == store.BreakScan {
			return store.BreakScan
		}

		if err != nil {
			return fmt.Errorf("on block ref for table %q key %q failed: %w", TblPrefixRows, key, err)
		}

		return nil
	})

	if err != nil && err != store.BreakScan {
		return fmt.Errorf("unable to determine if table %q has key prefix %q: %w", TblPrefixLast, keyPrefix, err)
	}

	return nil
}

func (s *KVStore) fetchKey(ctx context.Context, table byte, key string) (out []byte, err error) {

	kvKey := packKey(table, key)

	out, err = s.db.Get(ctx, kvKey)
	if err == kv.ErrNotFound {
		return nil, store.ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("unable to fetch table %q key %q: %w", TblPrefixName[table], key, err)
	}

	return out, nil
}

func (s *KVStore) fetchKeys(batchCtx context.Context, table byte, keys []string, onTabletRow store.OnTabletRow) error {
	batchCtx, cancelBatch := context.WithCancel(batchCtx)
	defer cancelBatch()

	kvKeys := make([][]byte, len(keys))
	for i, key := range keys {
		kvKeys[i] = packKey(table, key)
	}

	itr := s.db.BatchGet(batchCtx, kvKeys)

	for itr.Next() {
		value := itr.Item().Value
		// We must be prudent here, a `nil` value indicate a key not found, a `[]byte{}` indicates a found key without a value!
		if value == nil {
			continue
		}

		_, key := unpackKey(itr.Item().Key)
		err := onTabletRow(key, value)
		if err == store.BreakScan {
			return nil
		}

		if err != nil {
			return fmt.Errorf("on tablet row for key %q failed: %w", key, err)
		}
	}
	if err := itr.Err(); err != nil {
		return fmt.Errorf("unable to fetch table %q keys (%d): %w", TblPrefixName[table], len(keys), err)
	}

	return nil
}

func (s *KVStore) scanPrefix(ctx context.Context, table byte, prefixKey string, limit int, onRow func(key string, value []byte) error) error {
	kvPrefix := packKey(table, prefixKey)

	itrCtx, cancelIterator := context.WithCancel(ctx)
	defer cancelIterator()

	itr := s.db.Prefix(itrCtx, kvPrefix, limit)
	for itr.Next() {
		item := itr.Item()
		t, key := unpackKey(item.Key)
		err := onRow(key, item.Value)

		if err == store.BreakScan {
			return nil
		}

		if err != nil {
			return fmt.Errorf("scan prefix: unable to process for table %q with key %q: %w", TblPrefixName[t], key, err)
		}
	}
	if err := itr.Err(); err != nil {
		return fmt.Errorf("unable to scan table %q keys with prefix %q: %w", TblPrefixName[table], prefixKey, err)
	}

	return nil
}

func (s *KVStore) scanRange(ctx context.Context, table byte, keyStart, keyEnd string, limit int, onRow func(key string, value []byte) error) error {
	zlog.Debug("scanning range", zap.String("start", keyStart), zap.String("end", keyEnd))
	startKey := packKey(table, keyStart)
	var endKey []byte

	if keyEnd != "" {
		endKey = packKey(table, keyEnd)
	} else {
		// there is no key end key specified we go till the end of the table (1 byte more then the table prefix)
		endKey = []byte{table + 1}
	}

	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()

	itr := s.db.Scan(scanCtx, startKey, endKey, limit)

	for itr.Next() {
		item := itr.Item()
		t, key := unpackKey(item.Key)
		err := onRow(key, item.Value)
		if err == store.BreakScan {
			return nil
		}

		if err != nil {
			return fmt.Errorf("scan range: unable to process for table %q with key %q: %w", TblPrefixName[t], key, err)
		}
	}

	if err := itr.Err(); err != nil {
		return fmt.Errorf("unable to scan table %q keys with start key %q and end key %q: %w", TblPrefixName[table], keyStart, keyEnd, err)
	}

	return nil
}

func (s *KVStore) scanInfiniteRange(ctx context.Context, table byte, keyStart string, limit int, onRow func(key string, value []byte) error) error {
	return s.scanRange(ctx, table, keyStart, "", limit, onRow)
}

// There is most probably lots of repetition between this batch and the bigtable version.
// We should most probably improve the sharing by having a `baseBatch` struct or something
// like that.
type batch struct {
	store          *KVStore
	count          int
	tableMutations map[byte]map[string][]byte

	zlog *zap.Logger
}

func newBatch(store *KVStore, logger *zap.Logger) *batch {
	batchSet := &batch{store: store, zlog: logger}
	batchSet.Reset()

	return batchSet
}

func (b *batch) Reset() {
	b.count = 0
	b.tableMutations = map[byte]map[string][]byte{
		TblPrefixRows:  make(map[string][]byte),
		TblPrefixIndex: make(map[string][]byte),
		TblPrefixLast:  make(map[string][]byte),
	}
}

// For now, if flush each time we have 100 pending mutations in total, would need to be
// adjusted and to check if we would be able to improve throughput by using "batch" mode
// of bbolt (hopefully, exposed correctly in Hidalgo).
var maxMutationCount = 100

func (b *batch) FlushIfFull(ctx context.Context) error {
	if b.count <= maxMutationCount {
		// We are not there yet
		return nil
	}

	b.zlog.Debug("flushing a full batch set", zap.Int("count", b.count))
	if err := b.Flush(ctx); err != nil {
		return fmt.Errorf("flushing batch set: %w", err)
	}

	return nil
}

func (b *batch) Flush(ctx context.Context) error {
	ctx, span := dtracing.StartSpan(ctx, "flush batch set")
	defer span.End()

	b.zlog.Debug("flushing batch set")

	tableNames := []byte{
		TblPrefixRows,
		TblPrefixIndex,

		// The table name `last` must always be the last table in this list!
		TblPrefixLast,
	}

	// TODO: We could eventually parallelize this, but remember, last would need to be processed last, after all others!
	for _, tblName := range tableNames {
		muts := b.tableMutations[tblName]

		if len(muts) <= 0 {
			continue
		}

		b.zlog.Debug("applying bulk update", zap.String("table_name", TblPrefixName[tblName]), zap.Int("mutation_count", len(muts)))
		ctx, span := dtracing.StartSpan(ctx, "apply bulk updates", "table", tblName, "mutation_count", len(muts))

		for key, value := range muts {
			err := b.store.db.Put(ctx, packKey(tblName, key), value)
			if err != nil {
				return fmt.Errorf("unable to add table %q key %q to tx: %w", tblName, key, err)
			}
		}
		span.End()

	}

	err := b.store.db.FlushPuts(ctx)
	if err != nil {
		return fmt.Errorf("apply bulk: %w", err)
	}

	b.Reset()

	return nil
}

func (b *batch) setTable(table byte, key string, value []byte) {
	b.tableMutations[table][key] = value
	b.count++
}

func (b *batch) SetRow(key string, value []byte) {
	b.setTable(TblPrefixRows, key, value)
}

func (b *batch) SetLast(key string, value []byte) {
	b.setTable(TblPrefixLast, key, value)
}

func (b *batch) SetIndex(key string, tableSnapshot []byte) {
	b.setTable(TblPrefixIndex, key, tableSnapshot)
}

func packKey(table byte, key string) []byte {
	return append([]byte{table}, []byte(key)...)
}

func unpackKey(key []byte) (byte, string) {
	return key[0], string(key)
}
