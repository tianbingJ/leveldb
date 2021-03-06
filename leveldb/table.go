// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"bytes"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/syndtr/goleveldb/leveldb/cache"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/table"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// tFile holds basic information about a table.
// tfile 提供一个table的基础信息
// 最大key，最小key
type tFile struct {
	fd         storage.FileDesc
	seekLeft   int32
	size       int64
	imin, imax internalKey
}

// Returns true if given key is after largest key of this table.
// 指定的key比tFile的最大值还打
func (t *tFile) after(icmp *iComparer, ukey []byte) bool {
	return ukey != nil && icmp.uCompare(ukey, t.imax.ukey()) > 0
}

// Returns true if given key is before smallest key of this table.
// 指定的key比tFile的最小值还小
func (t *tFile) before(icmp *iComparer, ukey []byte) bool {
	return ukey != nil && icmp.uCompare(ukey, t.imin.ukey()) < 0
}

// Returns true if given key range overlaps with this table key range.
//判断用户key与tFile是否有交集
func (t *tFile) overlaps(icmp *iComparer, umin, umax []byte) bool {
	return !t.after(icmp, umin) && !t.before(icmp, umax)
}

// Cosumes one seek and return current seeks left.
// 消费seekLeft
func (t *tFile) consumeSeek() int32 {
	return atomic.AddInt32(&t.seekLeft, -1)
}

// Creates new tFile.
func newTableFile(fd storage.FileDesc, size int64, imin, imax internalKey) *tFile {
	f := &tFile{
		fd:   fd,
		size: size,
		imin: imin,
		imax: imax,
	}

	//一次查找等价16kb的压缩，查找次数足够多之后，开始压缩
	// We arrange to automatically compact this file after
	// a certain number of seeks.  Let's assume:
	//   (1) One seek costs 10ms
	//   (2) Writing or reading 1MB costs 10ms (100MB/s)
	//   (3) A compaction of 1MB does 25MB of IO:
	//         1MB read from this level
	//         10-12MB read from next level (boundaries may be misaligned)
	//         10-12MB written to next level
	// This implies that 25 seeks cost the same as the compaction
	// of 1MB of data.  I.e., one seek costs approximately the
	// same as the compaction of 40KB of data.  We are a little
	// conservative and allow approximately one seek for every 16KB
	// of data before triggering a compaction.
	f.seekLeft = int32(size / 16384)
	if f.seekLeft < 100 {
		f.seekLeft = 100
	}

	return f
}

//根据添加的文件信息生成tFile结构
func tableFileFromRecord(r atRecord) *tFile {
	return newTableFile(storage.FileDesc{Type: storage.TypeTable, Num: r.num}, r.size, r.imin, r.imax)
}

// tFiles hold multiple tFile.
type tFiles []*tFile

func (tf tFiles) Len() int      { return len(tf) }
func (tf tFiles) Swap(i, j int) { tf[i], tf[j] = tf[j], tf[i] }

//转成string
func (tf tFiles) nums() string {
	x := "[ "
	for i, f := range tf {
		if i != 0 {
			x += ", "
		}
		x += fmt.Sprint(f.fd.Num)
	}
	x += " ]"
	return x
}

// Returns true if i smallest key is less than j.
// This used for sort by key in ascending order.
func (tf tFiles) lessByKey(icmp *iComparer, i, j int) bool {
	a, b := tf[i], tf[j]
	n := icmp.Compare(a.imin, b.imin)
	if n == 0 {
		return a.fd.Num < b.fd.Num
	}
	return n < 0
}

// Returns true if i file number is greater than j.
// This used for sort by file number in descending order.
// 为什么不是小于而是大于？
func (tf tFiles) lessByNum(i, j int) bool {
	return tf[i].fd.Num > tf[j].fd.Num
}

// Sorts tables by key in ascending order.
func (tf tFiles) sortByKey(icmp *iComparer) {
	sort.Sort(&tFilesSortByKey{tFiles: tf, icmp: icmp})
}

// Sorts tables by file number in descending order.
func (tf tFiles) sortByNum() {
	sort.Sort(&tFilesSortByNum{tFiles: tf})
}

// Returns sum of all tables size.
func (tf tFiles) size() (sum int64) {
	for _, t := range tf {
		sum += t.size
	}
	return sum
}

// Searches smallest index of tables whose its smallest
// key is after or equal with given key.
// 查找tFiles里最小key大于internalKey里的tFile
func (tf tFiles) searchMin(icmp *iComparer, ikey internalKey) int {
	return sort.Search(len(tf), func(i int) bool {
		return icmp.Compare(tf[i].imin, ikey) >= 0
	})
}

// Searches smallest index of tables whose its largest
// key is after or equal with given key.
// 查找tfiles里最大key大于internakKey的tFile
func (tf tFiles) searchMax(icmp *iComparer, ikey internalKey) int {
	return sort.Search(len(tf), func(i int) bool {
		return icmp.Compare(tf[i].imax, ikey) >= 0
	})
}

// Searches smallest index of tables whose its file number
// is smaller than the given number.
// 找到文件编号小于num的最小的那个
func (tf tFiles) searchNumLess(num int64) int {
	return sort.Search(len(tf), func(i int) bool {
		return tf[i].fd.Num < num
	})
}

// Searches smallest index of tables whose its smallest
// key is after the given key.
// 二分查找
func (tf tFiles) searchMinUkey(icmp *iComparer, umin []byte) int {
	return sort.Search(len(tf), func(i int) bool {
		return icmp.ucmp.Compare(tf[i].imin.ukey(), umin) > 0
	})
}

// Searches smallest index of tables whose its largest
// key is after the given key.
func (tf tFiles) searchMaxUkey(icmp *iComparer, umax []byte) int {
	return sort.Search(len(tf), func(i int) bool {
		return icmp.ucmp.Compare(tf[i].imax.ukey(), umax) > 0
	})
}

// Returns true if given key range overlaps with one or more
// tables key range. If unsorted is true then binary search will not be used.
// 是否有覆盖:
func (tf tFiles) overlaps(icmp *iComparer, umin, umax []byte, unsorted bool) bool {
	if unsorted {
		// Check against all files.
		for _, t := range tf {
			if t.overlaps(icmp, umin, umax) {
				return true
			}
		}
		return false
	}

	i := 0
	if len(umin) > 0 {
		// Find the earliest possible internal key for min.
		i = tf.searchMax(icmp, makeInternalKey(nil, umin, keyMaxSeq, keyTypeSeek))
	}
	if i >= len(tf) {
		// Beginning of range is after all files, so no overlap.
		return false
	}
	return !tf[i].before(icmp, umax)
}

// Returns tables whose its key range overlaps with given key range.
// Range will be expanded if ukey found hop across tables.
// If overlapped is true then the search will be restarted if umax
// expanded.
// The dst content will be overwritten.
// 1.非0层没有重叠，使用二分查找在tFiles里查找 (overlapped = false)
// 使用二分查找找到所有相交的区间。
// 2.0层有重叠，需要一个一个遍历(overlapped == true)
// 不断用相交的区间扩充当前区间，返回的区间集合。 比如：umin, max = [3, 5], tf = [1, 4], [4, 7], [6,9]
// 结果会返回 [1, 4], [4, 7], [6,9]，即使[6,9]与[3,5]并没有直接的交集
func (tf tFiles) getOverlaps(dst tFiles, icmp *iComparer, umin, umax []byte, overlapped bool) tFiles {
	// Short circuit if tf is empty
	if len(tf) == 0 {
		return nil
	}
	// For non-zero levels, there is no ukey hop across at all.
	// And what's more, the files in these levels are strictly sorted,
	// so use binary search instead of heavy traverse.
	if !overlapped {
		var begin, end int
		// Determine the begin index of the overlapped file
		if umin != nil {
			index := tf.searchMinUkey(icmp, umin)
			if index == 0 {
				begin = 0
			} else if bytes.Compare(tf[index-1].imax.ukey(), umin) >= 0 {
				// The min ukey overlaps with the index-1 file, expand it.
				begin = index - 1
			} else {
				begin = index
			}
		}
		// Determine the end index of the overlapped file
		if umax != nil {
			index := tf.searchMaxUkey(icmp, umax)
			if index == len(tf) {
				end = len(tf)
			} else if bytes.Compare(tf[index].imin.ukey(), umax) <= 0 {
				// The max ukey overlaps with the index file, expand it.
				end = index + 1
			} else {
				end = index
			}
		} else {
			end = len(tf)
		}
		// Ensure the overlapped file indexes are valid.
		if begin >= end {
			return nil
		}
		dst = make([]*tFile, end-begin)
		copy(dst, tf[begin:end])
		return dst
	}

	//overlapped is true, level = 0
	dst = dst[:0]
	for i := 0; i < len(tf); {
		t := tf[i]
		if t.overlaps(icmp, umin, umax) {
			//以下两个会expand [umin, umax]的大小，但不会让i递增。
			if umin != nil && icmp.uCompare(t.imin.ukey(), umin) < 0 {
				umin = t.imin.ukey() //left expand umin
				dst = dst[:0]
				i = 0
				continue
			} else if umax != nil && icmp.uCompare(t.imax.ukey(), umax) > 0 {
				umax = t.imax.ukey()  //right expand umax
				// Restart search if it is overlapped.
				dst = dst[:0]
				i = 0
				continue
			}

			dst = append(dst, t)
		}
		i++
	}

	return dst
}

// Returns tables key range.
func (tf tFiles) getRange(icmp *iComparer) (imin, imax internalKey) {
	for i, t := range tf {
		if i == 0 {
			imin, imax = t.imin, t.imax
			continue
		}
		if icmp.Compare(t.imin, imin) < 0 {
			imin = t.imin
		}
		if icmp.Compare(t.imax, imax) > 0 {
			imax = t.imax
		}
	}

	return
}

// Creates iterator index from tables.
// 仅看slice.Range中的内容
func (tf tFiles) newIndexIterator(tops *tOps, icmp *iComparer, slice *util.Range, ro *opt.ReadOptions) iterator.IteratorIndexer {
	if slice != nil {
		var start, limit int
		if slice.Start != nil {
			start = tf.searchMax(icmp, internalKey(slice.Start))
		}
		if slice.Limit != nil {
			limit = tf.searchMin(icmp, internalKey(slice.Limit))
		} else {
			limit = tf.Len()
		}
		tf = tf[start:limit]
	}
	return iterator.NewArrayIndexer(&tFilesArrayIndexer{
		tFiles: tf,
		tops:   tops,
		icmp:   icmp,
		slice:  slice,
		ro:     ro,
	})
}

// Tables iterator index.
type tFilesArrayIndexer struct {
	tFiles
	tops  *tOps
	icmp  *iComparer
	slice *util.Range
	ro    *opt.ReadOptions
}

func (a *tFilesArrayIndexer) Search(key []byte) int {
	return a.searchMax(a.icmp, internalKey(key))
}

func (a *tFilesArrayIndexer) Get(i int) iterator.Iterator {
	if i == 0 || i == a.Len()-1 {
		return a.tops.newIterator(a.tFiles[i], a.slice, a.ro)
	}
	return a.tops.newIterator(a.tFiles[i], nil, a.ro)
}

// Helper type for sortByKey.
type tFilesSortByKey struct {
	tFiles
	icmp *iComparer
}

//比较最小值
func (x *tFilesSortByKey) Less(i, j int) bool {
	return x.lessByKey(x.icmp, i, j)
}

// Helper type for sortByNum.
type tFilesSortByNum struct {
	tFiles
}

//比较number从大到小
func (x *tFilesSortByNum) Less(i, j int) bool {
	return x.lessByNum(i, j)
}

// Table operations.
type tOps struct {
	s            *session
	noSync       bool
	evictRemoved bool
	//缓存, tFile的数据缓存
	cache        *cache.Cache
	//缓存
	bcache       *cache.Cache
	bpool        *util.BufferPool
}

// Creates an empty table and returns table writer.
func (t *tOps) create() (*tWriter, error) {
	fd := storage.FileDesc{Type: storage.TypeTable, Num: t.s.allocFileNum()}
	fw, err := t.s.stor.Create(fd)
	if err != nil {
		return nil, err
	}
	return &tWriter{
		t:  t,
		fd: fd,
		w:  fw,
		tw: table.NewWriter(fw, t.s.o.Options),
	}, nil
}

// Builds table from src iterator.
// 从内存生成tFile
func (t *tOps) createFrom(src iterator.Iterator) (f *tFile, n int, err error) {
	//创建tFile,并返回tableWriter
	w, err := t.create()
	if err != nil {
		return
	}

	defer func() {
		//生成文件错误，删除文件
		if err != nil {
			w.drop()
		}
	}()

	for src.Next() {
		err = w.append(src.Key(), src.Value())
		if err != nil {
			return
		}
	}
	err = src.Error()
	if err != nil {
		return
	}

	n = w.tw.EntriesLen()
	f, err = w.finish()
	return
}

// Opens table. It returns a cache handle, which should
// be released after use.
/*
返回值：
ch :
 */
func (t *tOps) open(f *tFile) (ch *cache.Handle, err error) {
	ch = t.cache.Get(0, uint64(f.fd.Num), func() (size int, value cache.Value) {
		var r storage.Reader
		r, err = t.s.stor.Open(f.fd)
		if err != nil {
			return 0, nil
		}

		var bcache *cache.NamespaceGetter
		if t.bcache != nil {
			bcache = &cache.NamespaceGetter{Cache: t.bcache, NS: uint64(f.fd.Num)}
		}

		var tr *table.Reader
		tr, err = table.NewReader(r, f.size, f.fd, bcache, t.bpool, t.s.o.Options)
		if err != nil {
			r.Close()
			return 0, nil
		}
		return 1, tr

	})
	if ch == nil && err == nil {
		err = ErrClosed
	}
	return
}

// Finds key/value pair whose key is greater than or equal to the
// given key.
/*
 找到大于等于当前key 的key value
 返回值：
	rkey: 右边的key
	rvalue: 右边的value
	err:
 */
func (t *tOps) find(f *tFile, key []byte, ro *opt.ReadOptions) (rkey, rvalue []byte, err error) {
	ch, err := t.open(f)
	if err != nil {
		return nil, nil, err
	}
	defer ch.Release()
	//value是table.Reader
	return ch.Value().(*table.Reader).Find(key, true, ro)
}

// Finds key that is greater than or equal to the given key.
//找到大于等于当前key的第一个key
//从SSTable中读取指定的key
func (t *tOps) findKey(f *tFile, key []byte, ro *opt.ReadOptions) (rkey []byte, err error) {
	ch, err := t.open(f)
	if err != nil {
		return nil, err
	}
	defer ch.Release()
	return ch.Value().(*table.Reader).FindKey(key, true, ro)
}

// Returns approximate offset of the given key.
func (t *tOps) offsetOf(f *tFile, key []byte) (offset int64, err error) {
	ch, err := t.open(f)
	if err != nil {
		return
	}
	defer ch.Release()
	return ch.Value().(*table.Reader).OffsetOf(key)
}

// Creates an iterator from the given table.
// 创建迭代器
func (t *tOps) newIterator(f *tFile, slice *util.Range, ro *opt.ReadOptions) iterator.Iterator {
	ch, err := t.open(f)
	if err != nil {
		return iterator.NewEmptyIterator(err)
	}
	iter := ch.Value().(*table.Reader).NewIterator(slice, ro)
	iter.SetReleaser(ch)
	return iter
}

// Removes table from persistent storage. It waits until
// no one use the the table.
func (t *tOps) remove(fd storage.FileDesc) {
	t.cache.Delete(0, uint64(fd.Num), func() {
		if err := t.s.stor.Remove(fd); err != nil {
			t.s.logf("table@remove removing @%d %q", fd.Num, err)
		} else {
			t.s.logf("table@remove removed @%d", fd.Num)
		}
		if t.evictRemoved && t.bcache != nil {
			t.bcache.EvictNS(uint64(fd.Num))
		}
		// Try to reuse file num, useful for discarded transaction.
		t.s.reuseFileNum(fd.Num)
	})
}

// Closes the table ops instance. It will close all tables,
// regadless still used or not.
func (t *tOps) close() {
	t.bpool.Close()
	t.cache.Close()
	if t.bcache != nil {
		t.bcache.CloseWeak()
	}
}

// Creates new initialized table ops instance.
func newTableOps(s *session) *tOps {
	var (
		//缓存读过sstable里的dataBlock
		cacher cache.Cacher
		//缓存打开的sstable文件句柄以及元数据
		bcache *cache.Cache
		bpool  *util.BufferPool
	)
	if s.o.GetOpenFilesCacheCapacity() > 0 {
		cacher = cache.NewLRU(s.o.GetOpenFilesCacheCapacity())
	}
	if !s.o.GetDisableBlockCache() {
		var bcacher cache.Cacher
		if s.o.GetBlockCacheCapacity() > 0 {
			bcacher = s.o.GetBlockCacher().New(s.o.GetBlockCacheCapacity())
		}
		bcache = cache.NewCache(bcacher)
	}
	if !s.o.GetDisableBufferPool() {
		bpool = util.NewBufferPool(s.o.GetBlockSize() + 5)
	}
	return &tOps{
		s:            s,
		noSync:       s.o.GetNoSync(),
		evictRemoved: s.o.GetBlockCacheEvictRemoved(),
		cache:        cache.NewCache(cacher),
		bcache:       bcache,
		bpool:        bpool,
	}
}

// tWriter wraps the table writer. It keep track of file descriptor
// and added key range.
type tWriter struct {
	t *tOps
	fd storage.FileDesc  //文件描述符
	w  storage.Writer    // 文件系统writer
	tw *table.Writer

	//first last是什么含义？
	//first last表示sstable的最小key和最大key
	first, last []byte
}

// Append key/value pair to the table.
func (w *tWriter) append(key, value []byte) error {
	if w.first == nil {
		w.first = append([]byte{}, key...)
	}
	w.last = append(w.last[:0], key...)
	return w.tw.Append(key, value)
}

// Returns true if the table is empty.
func (w *tWriter) empty() bool {
	return w.first == nil
}

// Closes the storage.Writer.
func (w *tWriter) close() {
	if w.w != nil {
		w.w.Close()
		w.w = nil
	}
}

// Finalizes the table and returns table file.
//
func (w *tWriter) finish() (f *tFile, err error) {
	defer w.close()
	err = w.tw.Close()
	if err != nil {
		return
	}
	if !w.t.noSync {
		err = w.w.Sync()
		if err != nil {
			return
		}
	}
	f = newTableFile(w.fd, int64(w.tw.BytesLen()), internalKey(w.first), internalKey(w.last))
	return
}

// Drops the table.
func (w *tWriter) drop() {
	w.close()
	w.t.s.stor.Remove(w.fd)
	w.t.s.reuseFileNum(w.fd.Num)
	w.tw = nil
	w.first = nil
	w.last = nil
}
