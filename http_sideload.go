package neutrino

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/neutrino/headerfs"
)

var (
	// httpClient is a default HTTP client used to download the side load
	// payload.
	httpClient = http.Client{
		Timeout: 5 * time.Second,
	}
)

// Status is a struct used to parse the JSON response of the block delivery
// network server's status response.
type Status struct {
	ChainGenesisHash string `json:"chain_genesis_hash"`
	ChainName        string `json:"chain_name"`
	BestBlockHeight  int32  `json:"best_block_height"`
	BestBlockHash    string `json:"best_block_hash"`
	EntriesPerHeader int32  `json:"entries_per_header"`
	EntriesPerFilter int32  `json:"entries_per_filter"`
}

// CheckChainHashMatches makes sure the status response matches the chain we are
// interested in.
func (s *Status) CheckChainHashMatches(chainParams *chaincfg.Params) error {
	genesisHash, err := chainhash.NewHashFromStr(s.ChainGenesisHash)
	if err != nil {
		return fmt.Errorf("unable to parse genesis hash: %w", err)
	}

	if *genesisHash != *chainParams.GenesisHash {
		return fmt.Errorf("genesis hash mismatch: expected %s, got %s",
			chainParams.GenesisHash, genesisHash)
	}

	return nil
}

// BestHeight returns the block hash and height of the status response.
func (s *Status) BestHeight() (*chainhash.Hash, int32, error) {
	bestHash, err := chainhash.NewHashFromStr(s.BestBlockHash)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to parse best hash: %w", err)
	}

	return bestHash, s.BestBlockHeight, nil
}

// sideLoadHeaders downloads the block headers and compact filter headers from
// the given base URL and stores them in the given header stores.
func sideLoadHeaders(sourceBaseURL string, chainParams *chaincfg.Params,
	headerStore headerfs.BlockHeaderStore,
	filterHeaderStore *headerfs.FilterHeaderStore) error {

	// We only do initial side loading if the filter header store is empty.
	_, bestHeight, err := headerStore.ChainTip()
	if err != nil {
		return err
	}

	if bestHeight > 0 {
		log.Debugf("Skipping initial HTTP assisted side load, header "+
			"store already synced to height %d", bestHeight)
		return nil
	}

	// We'll start by fetching the status of the remote server, so we can
	// verify that we're on the same chain and find out how many blocks
	// there are in total.
	status := Status{}
	statusURL := fmt.Sprintf("%s/status", sourceBaseURL)
	err = getJSON(statusURL, &status)
	if err != nil {
		return fmt.Errorf("error fetching HTTP side load server "+
			"status: %w", err)
	}

	err = status.CheckChainHashMatches(chainParams)
	if err != nil {
		return fmt.Errorf("http side load server on wrong chain: %w",
			err)
	}

	log.Debugf("Fetching block headers up to height %d",
		status.BestBlockHeight)

	// Fetch the block headers (80 bytes each) in batches. We can re-use
	// the wire block header and headerfs block header structs slices as
	// each batch is copied to the header store independently.
	var (
		headerBatchSize = status.EntriesPerHeader
		parsedHeaders   = make([]wire.BlockHeader, headerBatchSize)
		headerBatch     = make([]headerfs.BlockHeader, headerBatchSize)
	)
	err = batchLoad(
		sourceBaseURL, "%s/headers/%d", status.BestBlockHeight,
		headerBatchSize, func(startHeight int32, r io.Reader) error {
			return processHeaderBatch(
				startHeight, status.BestBlockHeight,
				headerBatchSize, r, parsedHeaders, headerBatch,
				headerStore,
			)
		},
	)
	if err != nil {
		return fmt.Errorf("error fetching headers: %w", err)
	}

	_, bestHeaderHeight, err := headerStore.ChainTip()
	if err != nil {
		return fmt.Errorf("error fetching header chain tip: %w", err)
	}

	log.Debugf("Fetching filter headers up to height %d", bestHeaderHeight)

	// Fetch the filter headers (32 bytes each) in batches.
	var (
		filterHeaderBatch = make(
			[]byte, headerBatchSize*chainhash.HashSize,
		)
		filterHash chainhash.Hash
	)
	err = batchLoad(
		sourceBaseURL, "%s/filter-headers/%d", int32(bestHeaderHeight),
		headerBatchSize, func(startHeight int32, r io.Reader) error {
			return processFilterHeaderBatch(
				startHeight, status.BestBlockHeight,
				headerBatchSize, r, &filterHash,
				filterHeaderBatch, headerStore,
				filterHeaderStore,
			)
		},
	)
	if err != nil {
		return fmt.Errorf("error fetching filter headers: %w", err)
	}

	return nil
}

// processHeaderBatch parses a batch of block headers from the given reader and
// writes them to the given header store. We pass in scratch buffers to avoid
// allocating them on each call. The store copies all data it needs, so we can
// re-use those buffers.
func processHeaderBatch(startHeight, bestHeight, headerBatchSize int32,
	r io.Reader, parseScratch []wire.BlockHeader,
	batchScratch []headerfs.BlockHeader,
	headerStore headerfs.BlockHeaderStore) error {

	var batchStartIndex, lastIndex int32
	for idx := int32(0); idx < headerBatchSize; idx++ {
		// Don't attempt to read past the best height.
		if startHeight+idx > bestHeight {
			break
		}

		err := parseScratch[idx].Deserialize(r)
		if err != nil {
			return fmt.Errorf("error deserializing header: %w", err)
		}

		// Special case for block 0, which is inserted into the header
		// store automatically when it's created, so we need to skip it.
		currentHeight := startHeight + idx
		if currentHeight == 0 {
			// We use index based access to the batch scratch space,
			// so since we skipped adding the first block, we need
			// to skip that in the batch as well.
			batchStartIndex = 1

			continue
		}

		batchScratch[idx] = headerfs.BlockHeader{
			BlockHeader: &parseScratch[idx],
			Height:      uint32(currentHeight),
		}

		lastIndex = idx
	}

	log.Debugf("Writing header batch up to height %d",
		lastIndex+startHeight)

	batchEnd := lastIndex + 1
	toWrite := batchScratch[batchStartIndex:batchEnd]

	log.Debugf("Writing %d headers, batch [%d:%d]", len(toWrite),
		batchStartIndex, batchEnd)
	return headerStore.WriteHeaders(toWrite...)
}

// processFilterHeaderBatch parses a batch of filter headers from the given
// reader and writes them to the given header store. We pass in scratch buffers
// to avoid allocating them on each call. The store copies all data it needs, so
// we can re-use those buffers.
func processFilterHeaderBatch(startHeight, bestHeight, headerBatchSize int32,
	r io.Reader, parseScratch *chainhash.Hash, batchScratch []byte,
	headerStore headerfs.BlockHeaderStore,
	filterHeaderStore *headerfs.FilterHeaderStore) error {

	var batchStartIndex, lastIndex int32
	for idx := int32(0); idx < headerBatchSize; idx++ {
		if startHeight+idx > bestHeight {
			break
		}

		n, err := io.ReadFull(r, parseScratch[:])
		if err != nil || n != chainhash.HashSize {
			return fmt.Errorf("error reading filter hash: %w", err)
		}

		// Special case for block 0, which is inserted into the filter
		// header store automatically.
		currentHeight := startHeight + idx
		if currentHeight == 0 {
			// We didn't write block 0 to the batch, so we'll also
			// skip it when writing the batch to the store.
			batchStartIndex = chainhash.HashSize

			continue
		}

		offset := idx * chainhash.HashSize
		copy(batchScratch[offset:], parseScratch[:])

		lastIndex = idx
	}

	lastHeight := lastIndex + startHeight
	header, err := headerStore.FetchHeaderByHeight(uint32(lastHeight))
	if err != nil {
		return fmt.Errorf("error fetching header for height %d: %w",
			lastHeight, err)
	}

	log.Debugf("Writing filter header batch up to height %d", lastHeight)

	batchEnd := (lastIndex + 1) * chainhash.HashSize
	toWrite := batchScratch[batchStartIndex:batchEnd]

	log.Debugf("Writing %d filters, batch bytes [%d:%d]", len(toWrite),
		batchStartIndex, batchEnd)
	return filterHeaderStore.WriteRaw(toWrite, header.BlockHash())
}

// validateSideLoadedHeaders makes sure the side loaded block headers and filter
// headers are valid and match our hard coded checkpoints.
func validateSideLoadedHeaders(chainCtx *lightChainCtx,
	timeSource blockchain.MedianTimeSource,
	headerStore headerfs.BlockHeaderStore,
	filterHeaderStore *headerfs.FilterHeaderStore) error {

	_, bestBlockHeight, err := headerStore.ChainTip()
	if err != nil {
		return fmt.Errorf("error fetching block header chain tip: %w",
			err)
	}

	_, bestFilterHeight, err := filterHeaderStore.ChainTip()
	if err != nil {
		return fmt.Errorf("error fetching filter header chain tip: %w",
			err)
	}

	if bestBlockHeight != bestFilterHeight {
		return fmt.Errorf("header store chain tip height %d does not "+
			"match filter header chain tip height %d",
			bestBlockHeight, bestFilterHeight)
	}

	var (
		prevHeader = chainCtx.params.GenesisBlock.Header
		currHeader wire.BlockHeader
	)
	for height := uint32(1); height <= bestBlockHeight; height++ {
		err := headerStore.FetchHeaderByHeightInto(height, &currHeader)
		if err != nil {
			return fmt.Errorf("error fetching header at height "+
				"%d: %w", height, err)
		}

		if currHeader.PrevBlock != prevHeader.BlockHash() {
			return fmt.Errorf("block header at height %d does "+
				"not refrence previous hash %v", height,
				prevHeader.BlockHash().String())
		}

		parentHeaderCtx := newLightHeaderCtx(
			int32(height-1), &prevHeader, headerStore, nil,
		)

		skipDifficulty := blockchain.BFFastAdd
		err = blockchain.CheckBlockHeaderContext(
			&currHeader, parentHeaderCtx, skipDifficulty, chainCtx,
			true,
		)
		if err != nil {
			return fmt.Errorf("error checking block %d header "+
				"context: %w", height, err)
		}

		err = blockchain.CheckBlockHeaderSanity(
			&currHeader, chainCtx.params.PowLimit, timeSource,
			skipDifficulty,
		)
		if err != nil {
			return fmt.Errorf("error checking block %d header "+
				"sanity: %w", height, err)
		}

		// TODO: Validate checkpoint and filter headers.
		prevHeader = currHeader

		if height > 0 && height%100_000 == 0 {
			log.Debugf("Validated %d headers", height)
		}
	}

	return nil
}

// batchLoad fetches resources from a base URL that supports batched queries and
// invokes the given callback for the data in each batch.
func batchLoad(baseURL, urlPattern string, bestHeight, entriesPerBatch int32,
	handler func(startHeight int32, r io.Reader) error) error {

	for i := int32(0); i <= bestHeight; i += entriesPerBatch {
		url := fmt.Sprintf(urlPattern, baseURL, i)

		log.Debugf("Fetching %s", url)

		resp, err := httpClient.Get(url)
		if err != nil {
			return fmt.Errorf("error fetching %s: %w", url, err)
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code %d",
				resp.StatusCode)
		}

		err = handler(i, resp.Body)
		if err != nil {
			return err
		}

		err = resp.Body.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// getJSON fetches the given URL and decodes the returned data as JSON.
func getJSON(url string, v interface{}) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("error fetching %s: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	err = json.NewDecoder(resp.Body).Decode(v)
	if err != nil {
		return fmt.Errorf("error decoding JSON: %w", err)
	}

	return nil
}
