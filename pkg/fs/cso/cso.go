package cso

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"syscall"

	"github.com/klauspost/compress/flate"
	"github.com/pierrec/lz4/v4"
	"github.com/xakep666/ps3netsrv-go/internal/handler"
)

type decompressor func(src, dst []byte) (int, error)

type File struct {
	f handler.File

	Header       *Header
	indexEntries *IndexEntryCache

	largeBlocksUncompressed bool         // if difference between two entry offsets >= block_size than block is uncompressed
	setBitDecompressor      decompressor // if entry top bit is 1
	clearBitDecompressor    decompressor // if entry top bit is 0

	offset   int64
	isClosed bool

	tmpBuf []byte

	cachedBlockNum int
	cachedBlock    []byte
}

func NewFile(f handler.File) (*File, error) {
	hdr, err := ReadHeader(f)
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	// sanity limits to avoid huge allocations and divisions by zero on corrupted headers
	const maxBlockSize = 16 << 20
	if hdr.BlockSize == 0 || hdr.BlockSize > maxBlockSize {
		return nil, fmt.Errorf("invalid block size %d", hdr.BlockSize)
	}
	if hdr.IndexShift > 31 {
		return nil, fmt.Errorf("invalid index shift %d", hdr.IndexShift)
	}

	// number taken from https://github.com/ps2homebrew/Open-PS2-Loader/blob/db4cb1f54045c231bc14227f035502d98e76ce60/modules/isofs/zso.h#L13
	const maxIndexEntries = 257

	ret := &File{
		f:              f,
		Header:         hdr,
		indexEntries:   NewIndexEntryCache(f, hdr, maxIndexEntries),
		cachedBlockNum: -1,
	}

	switch hdr.Variant() {
	case CSOv1:
		ret.clearBitDecompressor = flateDecompressor()
		ret.setBitDecompressor = rawDecompressor
	case CSOv2:
		if hdr.HeaderSize != AdvisedHeaderSize {
			return nil, fmt.Errorf("invalid header size %x", hdr.HeaderSize)
		}
		ret.clearBitDecompressor = flateDecompressor()
		ret.setBitDecompressor = lz4.UncompressBlock
		ret.largeBlocksUncompressed = true
	case ZSO:
		if hdr.HeaderSize != AdvisedHeaderSize {
			return nil, fmt.Errorf("invalid header size %x", hdr.HeaderSize)
		}
		ret.clearBitDecompressor = lz4.UncompressBlock
		ret.setBitDecompressor = rawDecompressor
	default:
		return nil, fmt.Errorf("unknown variant magic(%s)/version(%d)", hdr.Magic, hdr.Version)
	}

	return ret, nil
}

type csoStat struct {
	fs.FileInfo
	hdr *Header
}

func (s *csoStat) Size() int64 {
	return int64(s.hdr.UncompressedSize)
}

func (s *csoStat) Mode() fs.FileMode {
	return s.FileInfo.Mode() | fs.ModeIrregular
}

func (s *csoStat) Unwrap() fs.FileInfo {
	return s.FileInfo
}

func (f *File) Stat() (fs.FileInfo, error) {
	fi, err := f.f.Stat()
	if err != nil {
		return nil, err
	}
	return &csoStat{
		FileInfo: fi,
		hdr:      f.Header,
	}, nil
}

func (f *File) Read(b []byte) (int, error) {
	if f.isClosed {
		return 0, fs.ErrClosed
	}

	size := int64(f.Header.UncompressedSize)
	if f.offset >= size {
		return 0, io.EOF
	}

	if remaining := size - f.offset; int64(len(b)) > remaining {
		b = b[:remaining]
	}

	blockSize := int64(f.Header.BlockSize)
	var n int

	// Unaligned start or buffer smaller than a block: go through the cached block.
	if posInBlock := f.offset % blockSize; posInBlock > 0 || int64(len(b)) < blockSize {
		blockNum := int(f.offset / blockSize)
		if err := f.updateCachedBlock(blockNum); err != nil {
			return 0, err
		}

		n = copy(b, f.cachedBlock[posInBlock:f.blockLen(blockNum)])
		b = b[n:]
	}

	// Whole blocks are decompressed directly into the caller's buffer.
	startBlock := int((f.offset + int64(n)) / blockSize)
	if fullBlocks := len(b) / int(blockSize); fullBlocks > 0 {
		read, err := f.readFullBlocks(startBlock, fullBlocks, b[:int64(fullBlocks)*blockSize])
		n += read
		if err != nil {
			f.offset += int64(n)
			return n, err
		}

		b = b[read:]
		startBlock += fullBlocks
	}

	// Trailing partial block.
	if len(b) > 0 {
		if err := f.updateCachedBlock(startBlock); err != nil {
			f.offset += int64(n)
			return n, err
		}

		n += copy(b, f.cachedBlock[:f.blockLen(startBlock)])
	}

	f.offset += int64(n)
	return n, nil
}

// blockLen returns the uncompressed length of the block (the last one may be partial).
func (f *File) blockLen(blockNum int) int {
	blockSize := int64(f.Header.BlockSize)
	return int(min(blockSize, int64(f.Header.UncompressedSize)-int64(blockNum)*blockSize))
}

// blockSpan returns the compressed offset and stored size of the block.
func (f *File) blockSpan(blockNum int) (offset, size uint64, err error) {
	offset, err = f.indexEntries.OffsetOf(blockNum)
	if err != nil {
		return 0, 0, err
	}

	next, err := f.indexEntries.OffsetOf(blockNum + 1)
	if err != nil {
		return 0, 0, err
	}

	if next < offset || next-offset > f.maxStoredBlockSize() {
		return 0, 0, fmt.Errorf("corrupted index entry for block %d: [%d;%d)", blockNum, offset, next)
	}

	return offset, next - offset, nil
}

// maxStoredBlockSize is the upper bound for the stored size of one block: the uncompressed size plus alignment padding.
func (f *File) maxStoredBlockSize() uint64 {
	return uint64(f.Header.BlockSize) + 1<<f.Header.IndexShift
}

// readFullBlocks reads count blocks starting from startBlock into dst, every block must be complete.
func (f *File) readFullBlocks(startBlock, count int, dst []byte) (int, error) {
	startOffset, err := f.indexEntries.OffsetOf(startBlock)
	if err != nil {
		return 0, err
	}

	endOffset, err := f.indexEntries.OffsetOf(startBlock + count)
	if err != nil {
		return 0, err
	}

	if endOffset < startOffset || endOffset-startOffset > uint64(count)*f.maxStoredBlockSize() {
		return 0, fmt.Errorf("corrupted index entries for blocks [%d;%d)", startBlock, startBlock+count)
	}

	// read all compressed blocks with one syscall
	compressed := f.growTmpBuf(int(endOffset - startOffset))
	if _, err = f.f.Seek(int64(startOffset), io.SeekStart); err != nil {
		return 0, err
	}

	if _, err = io.ReadFull(f.f, compressed); err != nil {
		return 0, err
	}

	var n int
	for blockNum := startBlock; blockNum < startBlock+count; blockNum++ {
		blockOffset, storedSize, err := f.blockSpan(blockNum)
		if err != nil {
			return n, err
		}

		rel := blockOffset - startOffset
		blockLen := f.blockLen(blockNum)
		if err = f.decodeBlock(blockNum, compressed[rel:rel+storedSize], dst[n:n+blockLen]); err != nil {
			return n, err
		}

		n += blockLen
	}

	return n, nil
}

func (f *File) updateCachedBlock(blockNum int) error {
	if f.cachedBlockNum == blockNum {
		return nil
	}

	blockOffset, storedSize, err := f.blockSpan(blockNum)
	if err != nil {
		return err
	}

	f.cachedBlockNum = -1

	if _, err = f.f.Seek(int64(blockOffset), io.SeekStart); err != nil {
		return err
	}

	compressed := f.growTmpBuf(int(storedSize))
	if _, err = io.ReadFull(f.f, compressed); err != nil {
		return err
	}

	if len(f.cachedBlock) < int(f.Header.BlockSize) {
		f.cachedBlock = make([]byte, f.Header.BlockSize)
	}

	if err = f.decodeBlock(blockNum, compressed, f.cachedBlock); err != nil {
		return err
	}

	f.cachedBlockNum = blockNum
	return nil
}

// decodeBlock decompresses the stored block src into dst and checks that the whole block was produced.
func (f *File) decodeBlock(blockNum int, src, dst []byte) error {
	dec, err := f.selectDecompressor(blockNum, uint64(len(src)))
	if err != nil {
		return err
	}

	n, err := dec(src, dst)
	if err != nil {
		return fmt.Errorf("decompress block %d: %w", blockNum, err)
	}

	if want := min(len(dst), f.blockLen(blockNum)); n < want {
		return fmt.Errorf("decompress block %d: got %d bytes, want %d: %w", blockNum, n, want, io.ErrUnexpectedEOF)
	}

	return nil
}

func (f *File) growTmpBuf(size int) []byte {
	if cap(f.tmpBuf) < size {
		f.tmpBuf = make([]byte, size)
	}

	return f.tmpBuf[:size]
}

func (f *File) selectDecompressor(blockNum int, rawBlockSize uint64) (decompressor, error) {
	topBit, err := f.indexEntries.TopBitOf(blockNum)
	if err != nil {
		return nil, err
	}
	switch {
	case f.largeBlocksUncompressed && rawBlockSize >= uint64(f.Header.BlockSize):
		return rawDecompressor, nil
	case topBit:
		return f.setBitDecompressor, nil
	default:
		return f.clearBitDecompressor, nil
	}
}

func (f *File) Close() error {
	if f.isClosed {
		return fs.ErrClosed
	}

	f.isClosed = true
	return f.f.Close()
}

func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, errors.ErrUnsupported
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.isClosed {
		return 0, fs.ErrClosed
	}

	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += int64(f.offset)
	case io.SeekEnd:
		offset = int64(f.Header.UncompressedSize) + offset
	default:
		return 0, syscall.EINVAL
	}

	if offset < 0 || uint64(offset) > f.Header.UncompressedSize {
		return 0, syscall.EINVAL
	}

	f.offset = offset
	return offset, nil
}

func (f *File) Name() string {
	return f.f.Name()
}

func (f *File) Unwrap() handler.File {
	return f.f
}

func rawDecompressor(src, dst []byte) (int, error) {
	return copy(dst, src), nil
}

func flateDecompressor() decompressor {
	br := new(bytes.Reader)
	zr := flate.NewReader(nil)
	return func(src, dst []byte) (int, error) {
		br.Reset(src)
		if err := zr.(flate.Resetter).Reset(br, nil); err != nil {
			return -1, err
		}

		n, err := io.ReadFull(zr, dst)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return -1, err
		}

		if err = zr.Close(); err != nil {
			return -1, err
		}

		return n, nil
	}
}
