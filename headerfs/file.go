package headerfs

import (
	"bytes"
	"io"
	"os"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// ErrHeaderNotFound is returned when a target header on disk (flat file) can't
// be found.
type ErrHeaderNotFound struct {
	error
}

// appendRaw appends a new raw header to the end of the flat file.
func (h *headerStore) appendRaw(header []byte) error {
	if _, err := h.file.Write(header); err != nil {
		return err
	}

	return nil
}

// readRawBlockHeader reads a raw block header from disk at a particular height
// directly into the given block header.
func (h *headerStore) readRawBlockHeader(target *wire.BlockHeader,
	height uint32) error {

	// First, we'll seek to the location of the header in the file.
	seekDistance := int64(height) * wire.MaxBlockHeaderPayload
	if _, err := h.file.Seek(seekDistance, 0); err != nil {
		return &ErrHeaderNotFound{err}
	}

	// Next we get a buffer from our pool to read the header into. We can
	// avoid allocating a new buffer for each header by reusing buffers
	// from our pool.
	buf := headerBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer headerBufPool.Put(buf)

	// Now we read the header's bytes into our buffer. We don't directly
	// de-serialize the header into the target header because the
	// implementation does multiple reads which would mean multiple
	// individual disk accesses. Reading all 80 bytes in one go and then do
	// the de-serialization in-memory is much faster.
	n, err := buf.ReadFrom(
		io.LimitReader(h.file, wire.MaxBlockHeaderPayload),
	)
	if err != nil || n != wire.MaxBlockHeaderPayload {
		return &ErrHeaderNotFound{err}
	}

	// Finally, we de-serialize the header bytes into the target header.
	if err := target.Deserialize(buf); err != nil {
		return &ErrHeaderNotFound{err}
	}

	return nil
}

// readRawFilterHeader reads a raw filter header from disk at a particular
// height directly into the given filter header.
func (h *headerStore) readRawFilterHeader(target *chainhash.Hash,
	height uint32) error {

	seekDistance := int64(height) * chainhash.HashSize
	if _, err := h.file.Seek(seekDistance, 0); err != nil {
		return &ErrHeaderNotFound{err}
	}
	if _, err := io.ReadFull(h.file, target[:]); err != nil {
		return &ErrHeaderNotFound{err}
	}

	return nil
}

// readHeaderRange will attempt to fetch a series of block headers within the
// target height range. This method batches a set of reads into a single system
// call thereby increasing performance when reading a set of contiguous
// headers.
//
// NOTE: The end height is _inclusive_ so we'll fetch all headers from the
// startHeight up to the end height, including the final header.
func (h *blockHeaderStore) readHeaderRange(startHeight uint32,
	endHeight uint32) ([]wire.BlockHeader, error) {

	// Based on the defined header type, we'll determine the number of
	// bytes that we need to read from the file.
	headerReader, err := readHeadersFromFile(
		h.file, BlockHeaderSize, startHeight, endHeight,
	)
	if err != nil {
		return nil, err
	}

	// We'll now incrementally parse out the set of individual headers from
	// our set of serialized contiguous raw headers.
	numHeaders := endHeight - startHeight + 1
	headers := make([]wire.BlockHeader, 0, numHeaders)
	for headerReader.Len() != 0 {
		var nextHeader wire.BlockHeader
		if err := nextHeader.Deserialize(headerReader); err != nil {
			return nil, err
		}

		headers = append(headers, nextHeader)
	}

	return headers, nil
}

// readHeader reads a full block header from the flat-file. The header read is
// determined by the height value.
func (h *blockHeaderStore) readHeader(height uint32) (wire.BlockHeader, error) {
	var blockHeader wire.BlockHeader
	err := h.readRawBlockHeader(&blockHeader, height)
	return blockHeader, err
}

// readHeader reads a single filter header at the specified height from the
// flat files on disk.
func (f *FilterHeaderStore) readHeader(height uint32) (*chainhash.Hash, error) {
	var filterHeader chainhash.Hash
	err := f.readRawFilterHeader(&filterHeader, height)
	return &filterHeader, err
}

// readHeaderRange will attempt to fetch a series of filter headers within the
// target height range. This method batches a set of reads into a single system
// call thereby increasing performance when reading a set of contiguous
// headers.
//
// NOTE: The end height is _inclusive_ so we'll fetch all headers from the
// startHeight up to the end height, including the final header.
func (f *FilterHeaderStore) readHeaderRange(startHeight uint32,
	endHeight uint32) ([]chainhash.Hash, error) {

	// Based on the defined header type, we'll determine the number of
	// bytes that we need to read from the file.
	headerReader, err := readHeadersFromFile(
		f.file, RegularFilterHeaderSize, startHeight, endHeight,
	)
	if err != nil {
		return nil, err
	}

	// We'll now incrementally parse out the set of individual headers from
	// our set of serialized contiguous raw headers.
	numHeaders := endHeight - startHeight + 1
	headers := make([]chainhash.Hash, 0, numHeaders)
	for headerReader.Len() != 0 {
		var nextHeader chainhash.Hash
		if _, err := headerReader.Read(nextHeader[:]); err != nil {
			return nil, err
		}

		headers = append(headers, nextHeader)
	}

	return headers, nil
}

// readHeadersFromFile reads a chunk of headers, each of size headerSize, from
// the given file, from startHeight to endHeight.
func readHeadersFromFile(f *os.File, headerSize, startHeight,
	endHeight uint32) (*bytes.Reader, error) {

	// Each header is headerSize bytes, so using this information, we'll
	// seek a distance to cover that height based on the size the headers.
	seekDistance := uint64(startHeight) * uint64(headerSize)

	// Based on the number of headers in the range, we'll allocate a single
	// slice that's able to hold the entire range of headers.
	numHeaders := endHeight - startHeight + 1
	rawHeaderBytes := make([]byte, headerSize*numHeaders)

	// Now that we have our slice allocated, we'll read out the entire
	// range of headers with a single system call.
	_, err := f.ReadAt(rawHeaderBytes, int64(seekDistance))
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(rawHeaderBytes), nil
}
