package fluxdb

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"
	"time"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/bstream/forkable"
	"github.com/dfuse-io/dstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var shardsStore = os.Getenv("FLUXDB_SHARDING_STORE_PATH")

func TestSharding(t *testing.T) {
	dir := shardsStore
	if dir == "" {
		var err error
		dir, err = ioutil.TempDir("", "fluxdb-sharding-tests")
		require.NoError(t, err)
		defer func() {
			os.RemoveAll(dir)
		}()
	} else {
		os.RemoveAll(dir)
	}

	shardsStore, err := dstore.NewLocalStore(dir, "", "", true)
	require.NoError(t, err)

	shardCount := 2
	sharder := NewSharder(shardsStore, shardCount, 1, 3)

	tablet1 := newTestTablet("tb1")
	tablet2 := newTestTablet("tb2")

	singlet1 := newTestSinglet("sg1")
	singlet2 := newTestSinglet("sg2")

	streamBlock(t, sharder, "00000001aa", writeRequest(
		[]SingletEntry{singlet1.entry(t, 1, "s1 e #1")},
		[]TabletRow{tablet1.row(t, 1, "001", "t1 r1 #1"), tablet1.row(t, 1, "002", "t1 r2 #1")}),
	)

	streamBlock(t, sharder, "00000002aa", writeRequest(
		[]SingletEntry{singlet2.entry(t, 2, "s2 e #2")},
		[]TabletRow{tablet2.row(t, 2, "001", "t2 r1 #2"), tablet2.row(t, 2, "002", "t2 r2 #2")}),
	)

	streamBlock(t, sharder, "00000003aa", writeRequest(
		[]SingletEntry{singlet1.entry(t, 3, "s1 e #3"), singlet2.entry(t, 3, "s2 e #3")},
		[]TabletRow{
			tablet1.row(t, 3, "002", "t1 r2 #3"),

			tablet2.row(t, 3, "001", "t2 r1 #3"),
		}),
	)

	endBlock(t, sharder, "00000004aa")

	db, closer := NewTestDB(t)
	defer closer()

	db.shardCount = shardCount

	// Injection of each shard is done individually, each store pointing into the shard directory directly
	for i := 0; i < shardCount; i++ {
		db.shardIndex = i

		specificShardStore, err := dstore.NewLocalStore(path.Join(dir, fmt.Sprintf("%03d", i)), "", "", false)
		require.NoError(t, err)

		injector := NewShardInjector(specificShardStore, db)
		err = injector.Run()
		require.NoError(t, err, "Unable to reinject all shards correctly for shard index %03d", i)
	}

	singlet1Entry, err := db.ReadSingletEntryAt(context.Background(), singlet1, 3, nil)
	assert.Equal(t, singlet1.entry(t, 3, "s1 e #3"), singlet1Entry)

	singlet2Entry, err := db.ReadSingletEntryAt(context.Background(), singlet2, 3, nil)
	assert.Equal(t, singlet2.entry(t, 3, "s2 e #3"), singlet2Entry)

	tablet1Rows, err := db.ReadTabletAt(context.Background(), 3, tablet1, nil)
	assert.Equal(t, []TabletRow{tablet1.row(t, 1, "001", "t1 r1 #1"), tablet1.row(t, 3, "002", "t1 r2 #3")}, tablet1Rows)

	tablet2Rows, err := db.ReadTabletAt(context.Background(), 3, tablet2, nil)
	assert.Equal(t, []TabletRow{tablet2.row(t, 3, "001", "t2 r1 #3"), tablet2.row(t, 2, "002", "t2 r2 #2")}, tablet2Rows)
}

func streamBlock(t *testing.T, sharder *Sharder, id string, request *WriteRequest) {
	blk := bblock(id)
	request.Height = blk.Num()
	request.BlockRef = blk.AsRef()

	err := sharder.ProcessBlock(blk, fObj(request))
	require.NoError(t, err)
}

func endBlock(t *testing.T, sharder *Sharder, id string) {
	blk := bblock(id)
	req := &WriteRequest{
		Height:   blk.Num(),
		BlockRef: blk.AsRef(),
	}

	err := sharder.ProcessBlock(blk, fObj(req))
	require.Equal(t, ErrCleanSourceStop, err)
}

func writeRequest(entries []SingletEntry, rows []TabletRow) *WriteRequest {
	return &WriteRequest{
		SingletEntries: entries,
		TabletRows:     rows,
	}
}

func bblock(id string) *bstream.Block {
	ref := bstream.NewBlockRefFromID(id)
	fork := id[8:]

	return &bstream.Block{
		Id:         ref.ID(),
		Number:     ref.Num(),
		LibNum:     ref.Num() - 1,
		PreviousId: fmt.Sprintf("%08x%s", uint32(ref.Num()-1), fork),
		Timestamp:  time.Now(),
	}
}

func fObj(request *WriteRequest) *forkable.ForkableObject {
	return &forkable.ForkableObject{Step: forkable.StepIrreversible, Obj: request}
}