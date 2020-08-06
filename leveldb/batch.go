// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/memdb"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

// ErrBatchCorrupted records reason of batch corruption. This error will be
// wrapped with errors.ErrCorrupted.
type ErrBatchCorrupted struct {
	Reason string
}

func (e *ErrBatchCorrupted) Error() string {
	return fmt.Sprintf("leveldb: batch corrupted: %s", e.Reason)
}

func newErrBatchCorrupted(reason string) error {
	return errors.NewErrCorrupted(storage.FileDesc{}, &ErrBatchCorrupted{reason})
}

const (
	//batch其实的seq number(这里用8个字节) + batchLen(4个字节)
	batchHeaderLen = 8 + 4  //sequence number 和 entry的条数
	// batch扩容的阈值，如果记录数过多，>>> 3000条，则新增3000条的空间
	batchGrowRec   = 3000
)

// BatchReplay wraps basic batch operations.
// Batch操作的接口
type BatchReplay interface {
	Put(key, value []byte)
	Delete(key []byte)
}

//batch索引, batch索引有什么用？
type batchIndex struct {
	keyType            keyType
	keyPos, keyLen     int
	valuePos, valueLen int
}

//从data中读取key
func (index batchIndex) k(data []byte) []byte {
	return data[index.keyPos : index.keyPos+index.keyLen]
}

//从data中读取value
func (index batchIndex) v(data []byte) []byte {
	if index.valueLen != 0 {
		return data[index.valuePos : index.valuePos+index.valueLen]
	}
	return nil
}

//从data中读取key和value
func (index batchIndex) kv(data []byte) (key, value []byte) {
	return index.k(data), index.v(data)
}

// Batch is a write batch.
// 写journal时用到的batch
type Batch struct {
	//数据 是type1 key1 value1  tyep2 key2 value2.... 写在journal中也是按照这个顺序
	data  []byte
	//key pos value pos等在data中的位置
	index []batchIndex

	// internalLen is sums of key/value pair length plus 8-bytes internal key.
	//internalLen = 该批次中： key的总长度(+8 = seqence number(7) + type(1)) + value的总长度
	internalLen int
}

/**
预留出n字节的空间,如果剩余容量小于n.
*/
func (b *Batch) grow(n int) {
	o := len(b.data)
	if cap(b.data) - o < n {
		div := 1
		if len(b.index) > batchGrowRec {
			div = len(b.index) / batchGrowRec
		}
		// 如果batch中的entry数据少于3000， 则cap = 原来两倍的空间 + n
		// 如果batch中的entry书大于3000， 则cap = 大概3000条记录的大小 + n
		//len = o, cap = o + n + o/div
		ndata := make([]byte, o, o+n+o/div)
		copy(ndata, b.data)
		b.data = ndata
	}
}

func (b *Batch) appendRec(kt keyType, key, value []byte) {
	//为什么key和value要加上32位整数的序列化长度，这个整数是什么含义？
	//1又是什么？
	//ans:预先分配最大可能用到的空间，序列化后key的长度和value的长度暂时是不确定的， 1个字节是batch编码中表示操作type(put or del)的字节。
	//如果多分配了空间，方法最后通过b.data = data[:o]修正。
	n := 1 + binary.MaxVarintLen32 + len(key)
	if kt == keyTypeVal {
		n += binary.MaxVarintLen32 + len(value)
	}
	b.grow(n)
	index := batchIndex{keyType: kt}
	//o是操作的下标
	o := len(b.data)
	data := b.data[:o+n]
	//写入类型
	data[o] = byte(kt)
	o++
	//编码key的长度,并写入key的长度
	o += binary.PutUvarint(data[o:], uint64(len(key)))
	index.keyPos = o
	index.keyLen = len(key)
	//写入key
	o += copy(data[o:], key)
	//对value左类似处理
	if kt == keyTypeVal {
		o += binary.PutUvarint(data[o:], uint64(len(value)))
		index.valuePos = o
		index.valueLen = len(value)
		o += copy(data[o:], value)
	}
	b.data = data[:o]
	b.index = append(b.index, index)

	b.internalLen += index.keyLen + index.valueLen + 8
}

// Put appends 'put operation' of the given key/value pair to the batch.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) Put(key, value []byte) {
	b.appendRec(keyTypeVal, key, value)
}

// Delete appends 'delete operation' of the given key to the batch.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (b *Batch) Delete(key []byte) {
	b.appendRec(keyTypeDel, key, nil)
}

// Dump dumps batch contents. The returned slice can be loaded into the
// batch using Load method.
// The returned slice is not its own copy, so the contents should not be
// modified.
func (b *Batch) Dump() []byte {
	return b.data
}

// Load loads given slice into the batch. Previous contents of the batch
// will be discarded.
// The given slice will not be copied and will be used as batch buffer, so
// it is not safe to modify the contents of the slice.
//使用data作为Batch的data,构建index索引
//相当于重置当前Batch
func (b *Batch) Load(data []byte) error {
	return b.decode(data, -1)
}

// Replay replays batch contents.
// 好像只有测试用例用到了这个方法
func (b *Batch) Replay(r BatchReplay) error {
	for _, index := range b.index {
		switch index.keyType {
		case keyTypeVal:
			r.Put(index.k(b.data), index.v(b.data))
		case keyTypeDel:
			r.Delete(index.k(b.data))
		}
	}
	return nil
}

// Len returns number of records in the batch.
func (b *Batch) Len() int {
	return len(b.index)
}

// Reset resets the batch.
func (b *Batch) Reset() {
	b.data = b.data[:0]
	b.index = b.index[:0]
	b.internalLen = 0
}

//遍历batch中的数据，逐条调用fn方法处理
func (b *Batch) replayInternal(fn func(i int, kt keyType, k, v []byte) error) error {
	for i, index := range b.index {
		if err := fn(i, index.keyType, index.k(b.data), index.v(b.data)); err != nil {
			return err
		}
	}
	return nil
}

//当前batch追加另外一个批次
func (b *Batch) append(p *Batch) {
	//b中数据长度, p中的index索引需要加上ob
	ob := len(b.data)
	oi := len(b.index)
	b.data = append(b.data, p.data...)
	b.index = append(b.index, p.index...)
	b.internalLen += p.internalLen

	// Updating index offset.
	if ob != 0 {
		for ; oi < len(b.index); oi++ {
			index := &b.index[oi]
			index.keyPos += ob
			if index.valueLen != 0 {
				index.valuePos += ob
			}
		}
	}
}

/*
	使用data作为作为当前Batch的data.
    根据data更新Batch.index
	expectedLen:期望的entry数量
 */
func (b *Batch) decode(data []byte, expectedLen int) error {
	b.data = data
	b.index = b.index[:0]
	b.internalLen = 0
	err := decodeBatch(data, func(i int, index batchIndex) error {
		b.index = append(b.index, index)
		b.internalLen += index.keyLen + index.valueLen + 8
		return nil
	})
	if err != nil {
		return err
	}
	if expectedLen >= 0 && len(b.index) != expectedLen {
		return newErrBatchCorrupted(fmt.Sprintf("invalid records length: %d vs %d", expectedLen, len(b.index)))
	}
	return nil
}

/*
	放入到skip list中
	seq: 初始seq number，batch中的entry依次递增
*/
func (b *Batch) putMem(seq uint64, mdb *memdb.DB) error {
	var ik []byte
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		if err := mdb.Put(ik, index.v(b.data)); err != nil {
			return err
		}
	}
	return nil
}

//从memdb中删除这批数据
func (b *Batch) revertMem(seq uint64, mdb *memdb.DB) error {
	var ik []byte
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		if err := mdb.Delete(ik); err != nil {
			return err
		}
	}
	return nil
}

func newBatch() interface{} {
	return &Batch{}
}

// MakeBatch returns empty batch with preallocated buffer.
func MakeBatch(n int) *Batch {
	return &Batch{data: make([]byte, 0, n)}
}

/*
	根据data里的数据更新batchIndex
 */
func decodeBatch(data []byte, fn func(i int, index batchIndex) error) error {
	var index batchIndex
	for i, o := 0, 0; o < len(data); i++ {
		// Key type.
		index.keyType = keyType(data[o])
		if index.keyType > keyTypeVal {
			return newErrBatchCorrupted(fmt.Sprintf("bad record: invalid type %#x", uint(index.keyType)))
		}
		o++

		// Key. n是key的长度
		x, n := binary.Uvarint(data[o:])
		o += n
		if n <= 0 || o+int(x) > len(data) {
			return newErrBatchCorrupted("bad record: invalid key length")
		}
		index.keyPos = o
		index.keyLen = int(x)
		o += index.keyLen

		// Value.
		if index.keyType == keyTypeVal {
			x, n = binary.Uvarint(data[o:])
			o += n
			if n <= 0 || o+int(x) > len(data) {
				return newErrBatchCorrupted("bad record: invalid value length")
			}
			index.valuePos = o
			index.valueLen = int(x)
			o += index.valueLen
		} else {
			index.valuePos = 0
			index.valueLen = 0
		}

		if err := fn(i, index); err != nil {
			return err
		}
	}
	return nil
}

/*
解析data里的数据,逐条放入到memdb中
首先会解析出batch的其实sequence number，该值会和expectSeq比较。
 */
func decodeBatchToMem(data []byte, expectSeq uint64, mdb *memdb.DB) (seq uint64, batchLen int, err error) {
	seq, batchLen, err = decodeBatchHeader(data)
	if err != nil {
		return 0, 0, err
	}
	if seq < expectSeq {
		return 0, 0, newErrBatchCorrupted("invalid sequence number")
	}
	data = data[batchHeaderLen:]
	var ik []byte
	var decodedLen int
	err = decodeBatch(data, func(i int, index batchIndex) error {
		if i >= batchLen {
			return newErrBatchCorrupted("invalid records length")
		}
		ik = makeInternalKey(ik, index.k(data), seq+uint64(i), index.keyType)
		if err := mdb.Put(ik, index.v(data)); err != nil {
			return err
		}
		decodedLen++
		return nil
	})
	if err == nil && decodedLen != batchLen {
		err = newErrBatchCorrupted(fmt.Sprintf("invalid records length: %d vs %d", batchLen, decodedLen))
	}
	return
}

func encodeBatchHeader(dst []byte, seq uint64, batchLen int) []byte {
	dst = ensureBuffer(dst, batchHeaderLen)
	binary.LittleEndian.PutUint64(dst, seq)
	binary.LittleEndian.PutUint32(dst[8:], uint32(batchLen))
	return dst
}

func decodeBatchHeader(data []byte) (seq uint64, batchLen int, err error) {
	if len(data) < batchHeaderLen {
		return 0, 0, newErrBatchCorrupted("too short")
	}

	seq = binary.LittleEndian.Uint64(data)
	batchLen = int(binary.LittleEndian.Uint32(data[8:]))
	if batchLen < 0 {
		return 0, 0, newErrBatchCorrupted("invalid records length")
	}
	return
}

//所有批次的长度和
func batchesLen(batches []*Batch) int {
	batchLen := 0
	for _, batch := range batches {
		batchLen += batch.Len()
	}
	return batchLen
}

/*
	batches合并成一个batch，共用一个header
 */
func writeBatchesWithHeader(wr io.Writer, batches []*Batch, seq uint64) error {
	//write header
	if _, err := wr.Write(encodeBatchHeader(nil, seq, batchesLen(batches))); err != nil {
		return err
	}
	for _, batch := range batches {
		if _, err := wr.Write(batch.data); err != nil {
			return err
		}
	}
	return nil
}
