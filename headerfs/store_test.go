package headerfs

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/stretchr/testify/require"
)

func createTestBlockHeaderStore(
	t testing.TB) (walletdb.DB, string, *blockHeaderStore) {

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := walletdb.Create("bdb", dbPath, true, time.Second*10)
	require.NoError(t, err)

	hStore, err := NewBlockHeaderStore(tempDir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, db.Close())
		require.NoError(t, os.RemoveAll(tempDir))
	})

	return db, tempDir, hStore.(*blockHeaderStore)
}

func createTestBlockHeaderChain(numHeaders uint32) []BlockHeader {
	blockHeaders := make([]BlockHeader, numHeaders)
	prevHeader := &chaincfg.SimNetParams.GenesisBlock.Header
	for i := uint32(1); i <= numHeaders; i++ {
		bitcoinHeader := &wire.BlockHeader{
			Bits:      uint32(rand.Int31()),
			Nonce:     uint32(rand.Int31()),
			Timestamp: prevHeader.Timestamp.Add(time.Minute * 1),
			PrevBlock: prevHeader.BlockHash(),
		}

		blockHeaders[i-1] = BlockHeader{
			BlockHeader: bitcoinHeader,
			Height:      i,
		}

		prevHeader = bitcoinHeader
	}

	return blockHeaders
}

func TestBlockHeaderStoreOperations(t *testing.T) {
	_, _, bhs := createTestBlockHeaderStore(t)

	rand.Seed(time.Now().Unix())

	// With our test instance created, we'll now generate a series of
	// "fake" block headers to insert into the database.
	const numHeaders = 100
	blockHeaders := createTestBlockHeaderChain(numHeaders)

	// With all the headers inserted, we'll now insert them into the
	// database in a single batch.
	require.NoError(t, bhs.WriteHeaders(blockHeaders...))

	// At this point, the _tip_ of the chain from the PoV of the database
	// should be the very last header we inserted.
	lastHeader := blockHeaders[len(blockHeaders)-1]
	tipHeader, tipHeight, err := bhs.ChainTip()
	require.NoError(t, err)

	require.Equal(t, lastHeader.BlockHeader, tipHeader)
	require.Equal(t, lastHeader.Height, tipHeight)

	// Ensure that from the PoV of the database, the headers perfectly
	// connect.
	require.NoError(t, bhs.CheckConnectivity())

	// With all the headers written, we should be able to retrieve each
	// header according to its hash _and_ height.
	for _, header := range blockHeaders {
		dbHeader, err := bhs.FetchHeaderByHeight(header.Height)
		require.NoError(t, err)
		require.Equal(t, header.BlockHeader, dbHeader)

		blockHash := header.BlockHash()
		dbHeader, _, err = bhs.FetchHeader(&blockHash)
		require.NoError(t, err)
		require.Equal(t, dbHeader, header.BlockHeader)
	}

	// Finally, we'll test the roll back scenario. Roll back the chain by a
	// single block, the returned block stamp should exactly match the last
	// header inserted, and the current chain tip should be the second to
	// last header inserted.
	secondToLastHeader := blockHeaders[len(blockHeaders)-2]
	blockStamp, err := bhs.RollbackLastBlock()
	require.NoError(t, err)
	require.EqualValues(t, secondToLastHeader.Height, blockStamp.Height)

	headerHash := secondToLastHeader.BlockHash()
	require.Equal(t, headerHash, blockStamp.Hash)

	tipHeader, tipHeight, err = bhs.ChainTip()
	require.NoError(t, err)
	require.Equal(t, secondToLastHeader.BlockHeader, tipHeader)
	require.Equal(t, secondToLastHeader.Height, tipHeight)
}

// BenchmarkBlockHeaderStoreRead benchmarks the performance of reading headers
// from the database.
func BenchmarkBlockHeaderStoreRead(b *testing.B) {
	_, _, bhs := createTestBlockHeaderStore(b)

	// With our test instance created, we'll now generate a series of
	// "fake" block headers to insert into the database.
	const numHeaders = 100
	blockHeaders := createTestBlockHeaderChain(numHeaders)

	// With all the headers inserted, we'll now insert them into the
	// database in a single batch.
	require.NoError(b, bhs.WriteHeaders(blockHeaders...))

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for j := 1; j <= numHeaders; j++ {
			_, err := bhs.FetchHeaderByHeight(uint32(j))
			require.NoError(b, err)
		}
	}
}

func TestBlockHeaderStoreRecovery(t *testing.T) {
	// In this test we want to exercise the ability of the block header
	// store to recover in the face of a partial batch write (the headers
	// were written, but the index wasn't updated).
	db, tempDir, bhs := createTestBlockHeaderStore(t)

	// First we'll generate a test header chain of length 10, inserting it
	// into the header store.
	blockHeaders := createTestBlockHeaderChain(10)
	require.NoError(t, bhs.WriteHeaders(blockHeaders...))

	// Next, in order to simulate a partial write, we'll roll back the
	// internal index by 5 blocks.
	for i := 0; i < 5; i++ {
		newTip := blockHeaders[len(blockHeaders)-i-1].PrevBlock
		require.NoError(t, bhs.truncateIndex(&newTip, true))
	}

	// Next, we'll re-create the block header store in order to trigger the
	// recovery logic.
	hs, err := NewBlockHeaderStore(tempDir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)
	bhs = hs.(*blockHeaderStore)

	// The chain tip of this new instance should be of height 5, and match
	// the 5th to last block header.
	tipHash, tipHeight, err := bhs.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, 5, tipHeight)

	prevHeaderHash := blockHeaders[5].BlockHash()
	tipBlockHash := tipHash.BlockHash()
	require.NotEqual(t, prevHeaderHash, tipBlockHash)
}

func createTestFilterHeaderStore(
	t *testing.T) (walletdb.DB, string, *FilterHeaderStore) {

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	db, err := walletdb.Create("bdb", dbPath, true, time.Second*10)
	require.NoError(t, err)

	hStore, err := NewFilterHeaderStore(
		tempDir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, db.Close())
		require.NoError(t, os.RemoveAll(tempDir))
	})

	return db, tempDir, hStore
}

func createTestFilterHeaderChain(numHeaders uint32) []FilterHeader {
	filterHeaders := make([]FilterHeader, numHeaders)
	for i := uint32(1); i <= numHeaders; i++ {
		filterHeaders[i-1] = FilterHeader{
			HeaderHash: chainhash.DoubleHashH([]byte{byte(i)}),
			FilterHash: sha256.Sum256([]byte{byte(i)}),
			Height:     i,
		}
	}

	return filterHeaders
}

func TestFilterHeaderStoreOperations(t *testing.T) {
	_, _, fhs := createTestFilterHeaderStore(t)

	rand.Seed(time.Now().Unix())

	// With our test instance created, we'll now generate a series of
	// "fake" filter headers to insert into the database.
	const numHeaders = 100
	blockHeaders := createTestFilterHeaderChain(numHeaders)

	// We simulate the expected behavior of the block headers being written
	// to disk before the filter headers are.
	err := walletdb.Update(fhs.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(indexBucket)

		for _, header := range blockHeaders {
			entry := headerEntry{
				hash:   header.HeaderHash,
				height: header.Height,
			}
			err := putHeaderEntry(rootBucket, entry)
			if err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)

	// With all the headers inserted, we'll now insert half of them using
	// the regular insert.
	require.NoError(t, fhs.WriteHeaders(blockHeaders[:numHeaders/2]...))

	// We also want to test the raw insertion, for which we serialize the
	// second half of the headers and then insert them directly into the
	// file as a raw byte blob.
	var buf bytes.Buffer
	for _, header := range blockHeaders[numHeaders/2:] {
		_, err := buf.Write(header.FilterHash[:])
		require.NoError(t, err)
	}
	lastHeader := blockHeaders[len(blockHeaders)-1]
	require.NoError(t, fhs.WriteRaw(buf.Bytes(), lastHeader.HeaderHash))

	// At this point, the _tip_ of the chain from the PoV of the database
	// should be the very last header we inserted.
	tipHeader, tipHeight, err := fhs.ChainTip()
	require.NoError(t, err)
	require.Equal(t, lastHeader.FilterHash, *tipHeader)
	require.Equal(t, lastHeader.Height, tipHeight)

	// With all the headers written, we should be able to retrieve each
	// header according to its hash _and_ height.
	for _, header := range blockHeaders {
		dbHeader, err := fhs.FetchHeaderByHeight(header.Height)
		require.NoError(t, err)
		require.Equal(t, header.FilterHash, *dbHeader)

		blockHash := header.HeaderHash
		dbHeader, err = fhs.FetchHeader(&blockHash)
		require.NoError(t, err)
		require.Equal(t, header.FilterHash, *dbHeader)
	}

	// Finally, we'll test the roll back scenario. Roll back the chain by a
	// single block, the returned block stamp should exactly match the last
	// header inserted, and the current chain tip should be the second to
	// last header inserted.
	secondToLastHeader := blockHeaders[len(blockHeaders)-2]
	blockStamp, err := fhs.RollbackLastBlock(&secondToLastHeader.HeaderHash)
	require.NoError(t, err)
	require.EqualValues(t, secondToLastHeader.Height, blockStamp.Height)
	require.Equal(t, secondToLastHeader.FilterHash, blockStamp.Hash)

	tipHeader, tipHeight, err = fhs.ChainTip()
	require.NoError(t, err)
	require.Equal(t, secondToLastHeader.FilterHash, *tipHeader)
	require.Equal(t, secondToLastHeader.Height, tipHeight)
}

func TestFilterHeaderStoreRecovery(t *testing.T) {
	// In this test we want to exercise the ability of the filter header
	// store to recover in the face of a partial batch write (the headers
	// were written, but the index wasn't updated).
	db, tempDir, fhs := createTestFilterHeaderStore(t)

	blockHeaders := createTestFilterHeaderChain(10)

	// We simulate the expected behavior of the block headers being written
	// to disk before the filter headers are.
	err := walletdb.Update(fhs.db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(indexBucket)

		for _, header := range blockHeaders {
			entry := headerEntry{
				hash:   header.HeaderHash,
				height: header.Height,
			}
			err := putHeaderEntry(rootBucket, entry)
			if err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)

	// Next, we'll insert the filter header chain itself in to the
	// database.
	require.NoError(t, fhs.WriteHeaders(blockHeaders...))

	// Next, in order to simulate a partial write, we'll roll back the
	// internal index by 5 blocks.
	for i := 0; i < 5; i++ {
		newTip := blockHeaders[len(blockHeaders)-i-2].HeaderHash
		require.NoError(t, fhs.truncateIndex(&newTip, true))
	}

	// Next, we'll re-create the block header store in order to trigger the
	// recovery logic.
	fhs, err = NewFilterHeaderStore(
		tempDir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	// The chain tip of this new instance should be of height 5, and match
	// the 5th to last filter header.
	tipHash, tipHeight, err := fhs.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, 5, tipHeight)
	prevHeaderHash := blockHeaders[5].FilterHash
	require.NotEqual(t, prevHeaderHash, tipHash)
}

// TestBlockHeadersFetchHeaderAncestors tests that we're able to properly fetch
// the ancestors of a particular block, going from a set distance back to the
// target block.
func TestBlockHeadersFetchHeaderAncestors(t *testing.T) {
	t.Parallel()

	_, _, bhs := createTestBlockHeaderStore(t)

	rand.Seed(time.Now().Unix())

	// With our test instance created, we'll now generate a series of
	// "fake" block headers to insert into the database.
	const numHeaders = 100
	blockHeaders := createTestBlockHeaderChain(numHeaders)

	// With all the headers inserted, we'll now insert them into the
	// database in a single batch.
	require.NoError(t, bhs.WriteHeaders(blockHeaders...))

	// Now that the headers have been written to disk, we'll attempt to
	// query for all the ancestors of the final header written, to query
	// the entire range.
	lastHeader := blockHeaders[numHeaders-1]
	lastHash := lastHeader.BlockHash()
	diskHeaders, startHeight, err := bhs.FetchHeaderAncestors(
		numHeaders-1, &lastHash,
	)
	require.NoError(t, err)

	// Ensure that the first height of the block is height 1, and not the
	// genesis block.
	require.EqualValues(t, 1, startHeight)

	// Ensure that we retrieve the correct number of headers.
	require.Len(t, diskHeaders, numHeaders)

	// We should get back the exact same set of headers that we inserted in
	// the first place.
	for i := 0; i < len(diskHeaders); i++ {
		diskHeader := diskHeaders[i]
		blockHeader := blockHeaders[i].BlockHeader
		require.Equal(t, *blockHeader, diskHeader)
	}
}

// TestFilterHeaderStateAssertion tests that we'll properly delete or not
// delete the current on disk filter header state if a headerStateAssertion is
// passed in during initialization.
func TestFilterHeaderStateAssertion(t *testing.T) {
	t.Parallel()

	const chainTip = 10
	filterHeaderChain := createTestFilterHeaderChain(chainTip)

	setup := func(t *testing.T) (string, walletdb.DB) {
		db, tempDir, fhs := createTestFilterHeaderStore(t)

		// We simulate the expected behavior of the block headers being
		// written to disk before the filter headers are.
		err := walletdb.Update(
			fhs.db, func(tx walletdb.ReadWriteTx) error {
				rootBucket := tx.ReadWriteBucket(indexBucket)

				for _, header := range filterHeaderChain {
					entry := headerEntry{
						hash:   header.HeaderHash,
						height: header.Height,
					}
					err := putHeaderEntry(rootBucket, entry)
					if err != nil {
						return err
					}
				}

				return nil
			},
		)
		require.NoError(t, err)

		// Next we'll also write the chain to the flat file we'll make
		// our assertions against it.
		require.NoError(t, fhs.WriteHeaders(filterHeaderChain...))

		return tempDir, db
	}

	testCases := []struct {
		name            string
		headerAssertion *FilterHeader
		shouldRemove    bool
	}{
		// A header that we know already to be in our local chain. It
		// shouldn't remove the state.
		{
			name:            "correct assertion",
			headerAssertion: &filterHeaderChain[3],
			shouldRemove:    false,
		},

		// A made up header that isn't in the chain. It should remove
		// all state.
		{
			name: "incorrect assertion",
			headerAssertion: &FilterHeader{
				Height: 5,
			},
			shouldRemove: true,
		},

		// A header that's well beyond the chain height, it should be a
		// noop.
		{
			name: "assertion not found",
			headerAssertion: &FilterHeader{
				Height: 500,
			},
			shouldRemove: false,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		success := t.Run(testCase.name, func(t *testing.T) {
			// We'll start the test by setting up our required
			// dependencies.
			tempDir, db := setup(t)

			// We'll then re-initialize the filter header store with
			// its expected assertion.
			fhs, err := NewFilterHeaderStore(
				tempDir, db, RegularFilter,
				&chaincfg.SimNetParams,
				testCase.headerAssertion,
			)
			require.NoError(t, err)

			// If the assertion failed, we should expect the tip of
			// the chain to no longer exist as the state should've
			// been removed.
			_, err = fhs.FetchHeaderByHeight(chainTip)
			if testCase.shouldRemove {
				require.Error(t, err)
				require.IsType(t, &ErrHeaderNotFound{}, err)
			} else {
				require.NoError(t, err)
			}
		})
		if !success {
			break
		}
	}
}

// TODO(roasbeef): combined re-org scenarios
