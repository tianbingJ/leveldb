// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package table

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/golang/snappy"

	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

//求出a和b前面相同的字节数
func sharedPrefixLen(a, b []byte) int {
	i, n := 0, len(a)
	if n > len(b) {
		n = len(b)
	}
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

//sstable由data blocks : Filter blocks : Meta Index Blocks : Index Block : Footer
//每个block由entries:restartPoints:Restart Point Length组成
type blockWriter struct {
	restartInterval int    //默认每16个key使用一个公共前缀
	buf             util.Buffer  //存储entry的内容
	nEntries        int    //条目个数
	prevKey         []byte  //前一条key，用户计算两个key之间的共享前缀
	restarts        []uint32  //restart points
	scratch         []byte   //scratch表示几个长度的拼接 = shared key len:unshared key len : value len
}

//把key value添加到data block中
func (w *blockWriter) append(key, value []byte) {
	nShared := 0
	//使用新的共享前缀
	if w.nEntries%w.restartInterval == 0 {
		w.restarts = append(w.restarts, uint32(w.buf.Len()))
	} else {
		nShared = sharedPrefixLen(w.prevKey, key)
	}
	n := binary.PutUvarint(w.scratch[0:], uint64(nShared))
	n += binary.PutUvarint(w.scratch[n:], uint64(len(key)-nShared))
	n += binary.PutUvarint(w.scratch[n:], uint64(len(value)))
	w.buf.Write(w.scratch[:n])
	w.buf.Write(key[nShared:])
	w.buf.Write(value)
	w.prevKey = append(w.prevKey[:0], key...)
	w.nEntries++
}

// 把restart points写入buf中
func (w *blockWriter) finish() {
	// Write restarts entry.
	if w.nEntries == 0 {
		// Must have at least one restart entry.
		// 至少有一个restartPoints
		w.restarts = append(w.restarts, 0)
	}

	//restarts的长度添加到列表中.
	w.restarts = append(w.restarts, uint32(len(w.restarts)))
	//写入buffer
	for _, x := range w.restarts {
		buf4 := w.buf.Alloc(4)
		binary.LittleEndian.PutUint32(buf4, x)
	}
}

func (w *blockWriter) reset() {
	w.buf.Reset()
	w.nEntries = 0
	w.restarts = w.restarts[:0]
}

//整个block的长度 = entry的长度 + restart point的个数 + reset point的offset
func (w *blockWriter) bytesLen() int {
	restartsLen := len(w.restarts)
	if restartsLen == 0 {
		restartsLen = 1
	}
	return w.buf.Len() + 4*restartsLen + 4
}

//过滤器(布隆)
type filterWriter struct {
	generator filter.FilterGenerator
	buf       util.Buffer  //缓存过滤器data的buffer
	nKeys     int
	offsets   []uint32   //每个filter data的偏移地址
}

//添加key到布隆过滤器中
func (w *filterWriter) add(key []byte) {
	if w.generator == nil {
		return
	}
	w.generator.Add(key)
	w.nKeys++
}

// w.generate()方法会根据根据过滤器里的内容产生新的数据追加到buf中
func (w *filterWriter) flush(offset uint64) {
	if w.generator == nil {
		return
	}
	for x := int(offset / filterBase); x > len(w.offsets); {
		w.generate()
	}
}

// 结束filter部分数据
// filter部分会在sstable文件结束时才会写入文件
func (w *filterWriter) finish() {
	if w.generator == nil {
		return
	}
	// Generate last keys.
	// 对最后的数据生成过filter data
	if w.nKeys > 0 {
		w.generate()
	}
	//写入整个filter 索引数据的的起始地址(filter data部分的结束地址)
	w.offsets = append(w.offsets, uint32(w.buf.Len()))
	for _, x := range w.offsets {
		buf4 := w.buf.Alloc(4)
		binary.LittleEndian.PutUint32(buf4, x)
	}
	w.buf.WriteByte(filterBaseLg)
}

// 把filter data写入buf中
func (w *filterWriter) generate() {
	// Record offset.
	// 记录该过滤器的偏移地址
	w.offsets = append(w.offsets, uint32(w.buf.Len()))
	// Generate filters.
	if w.nKeys > 0 {
		//根据添加的key生成过滤器data，放入buffer中
		//重置过滤器
		w.generator.Generate(&w.buf)
		w.nKeys = 0
	}
}

// Writer is a table writer.
type Writer struct {
	writer io.Writer
	err    error
	// Options
	cmp         comparer.Comparer
	filter      filter.Filter
	compression opt.Compression
	blockSize   int     //默认4KB

	//dataBlock和indexBlock共用一个blockWriter
	dataBlock   blockWriter  //data块writer
	indexBlock  blockWriter  //indexBlock块 writer

	filterBlock filterWriter  //filter块writer
	pendingBH   blockHandle  //上一个block块的索引信息, 当下一个block块开始写入的时候，将该索引信息写入indexBlock中
	offset      uint64      // 当前block在文件中的其实偏移信息
	nEntries    int   //entry的数量，添加的时候统计
	// Scratch allocated enough for 5 uvarint. Block writer should not use
	// first 20-bytes since it will be used to encode block handle, which
	// then passed to the block writer itself.
	scratch            [50]byte
	comparerScratch    []byte
	compressionScratch []byte
}

//如果需要的话，压缩block
func (w *Writer) writeBlock(buf *util.Buffer, compression opt.Compression) (bh blockHandle, err error) {
	// Compress the buffer if necessary.
	var b []byte
	if compression == opt.SnappyCompression {
		// Allocate scratch enough for compression and block trailer.
		if n := snappy.MaxEncodedLen(buf.Len()) + blockTrailerLen; len(w.compressionScratch) < n {
			w.compressionScratch = make([]byte, n)
		}
		compressed := snappy.Encode(w.compressionScratch, buf.Bytes())
		n := len(compressed)
		b = compressed[:n+blockTrailerLen]
		b[n] = blockTypeSnappyCompression
	} else {
		//不压缩
		tmp := buf.Alloc(blockTrailerLen)
		tmp[0] = blockTypeNoCompression
		b = buf.Bytes()
	}

	// Calculate the checksum.
	n := len(b) - 4
	checksum := util.NewCRC(b[:n]).Value()
	binary.LittleEndian.PutUint32(b[n:], checksum)

	// Write the buffer to the file.
	// 真正写入文件
	_, err = w.writer.Write(b)
	if err != nil {
		return
	}
	//bh 是该block在文件中的偏移信息和数据的长度
	bh = blockHandle{w.offset, uint64(len(b) - blockTrailerLen)}
	//更新文件偏移地址offset
	w.offset += uint64(len(b))
	return
}

//刷新block handle到block index中
//block index包括：该block key的最大值 + 偏移量 + block size
func (w *Writer) flushPendingBH(key []byte) {
	if w.pendingBH.length == 0 {
		return
	}
	var separator []byte
	if len(key) == 0 {
		separator = w.cmp.Successor(w.comparerScratch[:0], w.dataBlock.prevKey)
	} else {
		separator = w.cmp.Separator(w.comparerScratch[:0], w.dataBlock.prevKey, key)
	}
	if separator == nil {
		separator = w.dataBlock.prevKey
	} else {
		w.comparerScratch = separator
	}
	n := encodeBlockHandle(w.scratch[:20], w.pendingBH)
	// Append the block handle to the index block.
	w.indexBlock.append(separator, w.scratch[:n])
	// Reset prev key of the data block.
	w.dataBlock.prevKey = w.dataBlock.prevKey[:0]
	// Clear pending block handle.
	w.pendingBH = blockHandle{}
}

// 把data block写入磁盘
func (w *Writer) finishBlock() error {
	//data block不再写入数据, 把restartPoint信息写入buf中
	w.dataBlock.finish()
	//写入磁盘, 返回bh:该block的offset和长度信息
	bh, err := w.writeBlock(&w.dataBlock.buf, w.compression)
	if err != nil {
		return err
	}
	w.pendingBH = bh
	// Reset the data block.
	w.dataBlock.reset()
	// Flush the filter block.
	// 重新构造一个Filter Data？, w.offset是新data block的起始偏移地址
	w.filterBlock.flush(w.offset)
	return nil
}

// Append appends key/value pair to the table. The keys passed must
// be in increasing order.
//
// It is safe to modify the contents of the arguments after Append returns.
// 把key和value添加到table中, 函数返回可以可以安全修改参数的内容。
func (w *Writer) Append(key, value []byte) error {
	if w.err != nil {
		return w.err
	}
	//如果当前entry不必前一条entry大，说明不是顺序写入，报错
	if w.nEntries > 0 && w.cmp.Compare(w.dataBlock.prevKey, key) >= 0 {
		w.err = fmt.Errorf("leveldb/table: Writer: keys are not in increasing order: %q, %q", w.dataBlock.prevKey, key)
		return w.err
	}

	w.flushPendingBH(key)
	// Append key/value pair to the data block.
	// 数据库添加key,value
	w.dataBlock.append(key, value)
	// Add key to the filter block.
	w.filterBlock.add(key)

	// Finish the data block if block size target reached.
	// 数据数据大小超过了设置的值，写入磁盘
	if w.dataBlock.bytesLen() >= w.blockSize {
		if err := w.finishBlock(); err != nil {
			w.err = err
			return w.err
		}
	}
	w.nEntries++
	return nil
}

// BlocksLen returns number of blocks written so far.
// blocks的数量
func (w *Writer) BlocksLen() int {
	n := w.indexBlock.nEntries
	if w.pendingBH.length > 0 {
		// Includes the pending block.
		n++
	}
	return n
}

// EntriesLen returns number of entries added so far.
func (w *Writer) EntriesLen() int {
	return w.nEntries
}

// BytesLen returns number of bytes written so far.
// 目前已经写入的数据量
func (w *Writer) BytesLen() int {
	return int(w.offset)
}

// Close will finalize the table. Calling Append is not possible
// after Close, but calling BlocksLen, EntriesLen and BytesLen
// is still possible.
// 前面append的时候只往sstable里写入了data block的数据，close的时候需要把filter数据、index数据、footer等写入文件中
func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}

	// Write the last data block. Or empty data block if there
	// aren't any data blocks at all.
	if w.dataBlock.nEntries > 0 || w.nEntries == 0 {
		if err := w.finishBlock(); err != nil {
			w.err = err
			return w.err
		}
	}
	w.flushPendingBH(nil)

	// Write the filter block.
	var filterBH blockHandle
	w.filterBlock.finish()
	if buf := &w.filterBlock.buf; buf.Len() > 0 {
		filterBH, w.err = w.writeBlock(buf, opt.NoCompression)
		if w.err != nil {
			return w.err
		}
	}

	// Write the metaindex block.
	if filterBH.length > 0 {
		key := []byte("filter." + w.filter.Name())
		n := encodeBlockHandle(w.scratch[:20], filterBH)
		w.dataBlock.append(key, w.scratch[:n])
	}
	w.dataBlock.finish()
	metaindexBH, err := w.writeBlock(&w.dataBlock.buf, w.compression)
	if err != nil {
		w.err = err
		return w.err
	}

	// Write the index block.
	w.indexBlock.finish()
	indexBH, err := w.writeBlock(&w.indexBlock.buf, w.compression)
	if err != nil {
		w.err = err
		return w.err
	}

	// Write the table footer.
	footer := w.scratch[:footerLen]
	for i := range footer {
		footer[i] = 0
	}
	n := encodeBlockHandle(footer, metaindexBH)
	encodeBlockHandle(footer[n:], indexBH)
	copy(footer[footerLen-len(magic):], magic)
	if _, err := w.writer.Write(footer); err != nil {
		w.err = err
		return w.err
	}
	w.offset += footerLen

	w.err = errors.New("leveldb/table: writer is closed")
	return nil
}

// NewWriter creates a new initialized table writer for the file.
//
// Table writer is not safe for concurrent use.
func NewWriter(f io.Writer, o *opt.Options) *Writer {
	w := &Writer{
		writer:          f,
		cmp:             o.GetComparer(),
		filter:          o.GetFilter(),
		compression:     o.GetCompression(),
		blockSize:       o.GetBlockSize(),
		comparerScratch: make([]byte, 0),
	}
	// data block
	//共享key的restart point interval
	w.dataBlock.restartInterval = o.GetBlockRestartInterval()
	// The first 20-bytes are used for encoding block handle.
	// 不是用前20个字节
	w.dataBlock.scratch = w.scratch[20:]
	// index block
	w.indexBlock.restartInterval = 1
	w.indexBlock.scratch = w.scratch[20:]
	// filter block
	if w.filter != nil {
		w.filterBlock.generator = w.filter.NewGenerator()
		w.filterBlock.flush(0)
	}
	return w
}
