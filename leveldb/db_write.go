// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

/*
    控制写入内存、journal核心的逻辑。
 */
import (
	"sync/atomic"
	"time"

	"github.com/syndtr/goleveldb/leveldb/memdb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

//把batch写入Journal中
func (db *DB) writeJournal(batches []*Batch, seq uint64, sync bool) error {
	wr, err := db.journal.Next()
	if err != nil {
		return err
	}
	if err := writeBatchesWithHeader(wr, batches, seq); err != nil {
		return err
	}
	if err := db.journal.Flush(); err != nil {
		return err
	}
	if sync {
		return db.journalWriter.Sync()
	}
	return nil
}

/*
	当前memdb变成frozen mem，分配新的journal
	当前journal fd变成frozen journal fd
	wait: 是否等待压实返回
 */
func (db *DB) rotateMem(n int, wait bool) (mem *memDB, err error) {
	retryLimit := 3
retry:
	// Wait for pending memdb compaction.
	err = db.compTriggerWait(db.mcompCmdC)
	if err != nil {
		return
	}
	retryLimit--

	// Create new memdb and journal.
	mem, err = db.newMem(n)
	if err != nil {
		if err == errHasFrozenMem {
			if retryLimit <= 0 {
				panic("BUG: still has frozen memdb")
			}
			goto retry
		}
		return
	}

	// Schedule memdb compaction.
	// 同步等待
	if wait {
		err = db.compTriggerWait(db.mcompCmdC)
	} else {
		// 不等待结果
		db.compTrigger(db.mcompCmdC)
	}
	return
}

/*
flush确保mdb中至少有n的空间。
 */
func (db *DB) flush(n int) (mdb *memDB, mdbFree int, err error) {
	delayed := false
	slowdownTrigger := db.s.o.GetWriteL0SlowdownTrigger()
	pauseTrigger := db.s.o.GetWriteL0PauseTrigger()
	flush := func() (retry bool) {
		//获取当前memtable
		mdb = db.getEffectiveMem()
		if mdb == nil {
			err = ErrClosed
			return false
		}
		defer func() {
			if retry {
				mdb.decref()
				mdb = nil
			}
		}()
		//level0 tfiles个数
		tLen := db.s.tLen(0)
		mdbFree = mdb.Free()
		//
		switch {
		case tLen >= slowdownTrigger && !delayed: //如果大于slowdownTrigger，先暂停1ms. 如果已经delayed过了，则不暂停
			delayed = true
			time.Sleep(time.Millisecond)
		case mdbFree >= n:  //如果mdbFree > n，则表示有足够的空间不需要刷新
			return false
		case tLen >= pauseTrigger: //大于pause Trigger则停止写入

			delayed = true
			// Set the write paused flag explicitly.
			atomic.StoreInt32(&db.inWritePaused, 1)
			//等待compaction结束
			err = db.compTriggerWait(db.tcompCmdC)
			// Unset the write paused flag.
			atomic.StoreInt32(&db.inWritePaused, 0)
			if err != nil {
				return false
			}
		default:
			// Allow memdb to grow if it has no entry.
			//这里没有看明白，如果当前skipList中没有entry，为什么mdbFree直接 = n？
			if mdb.Len() == 0 {
				mdbFree = n
			} else {
				mdb.decref()
				mdb, err = db.rotateMem(n, false)
				if err == nil {
					mdbFree = mdb.Free()
				} else {
					mdbFree = 0
				}
			}
			return false
		}
		return true
	}
	start := time.Now()
	for flush() {
	}
	if delayed {
		db.writeDelay += time.Since(start)
		db.writeDelayN++
	} else if db.writeDelayN > 0 {
		db.logf("db@write was delayed N·%d T·%v", db.writeDelayN, db.writeDelay)
		atomic.AddInt32(&db.cWriteDelayN, int32(db.writeDelayN))
		atomic.AddInt64(&db.cWriteDelay, int64(db.writeDelay))
		db.writeDelay = 0
		db.writeDelayN = 0
	}
	return
}


/*
写合并的结构
 */
type writeMerge struct {
	//是否同步写磁盘
	sync       bool
	//批次信息
	batch      *Batch

	//写单条数据用到如下信息
	keyType    keyType
	key, value []byte
}

/*
	err可以是nil
 */
func (db *DB) unlockWrite(overflow bool, merged int, err error) {
	for i := 0; i < merged; i++ {
		db.writeAckC <- err  //通知写ack， err != nil表示写失败
	}
	//溢出时最后一个的写入操作没有被merge，写入的goroutine依然在等待，这里通知false，进而它会获取锁?
	if overflow {
		// Pass lock to the next write (that failed to merge).
		db.writeMergedC <- false
	} else {
		// Release lock.
		<-db.writeLockC
	}
}

// ourBatch is batch that we can modify.
/*
	调用这个方法需要保证已经获取到锁
	merge: 是否合并其他待写入的数据
	batch: 要写入的batch,这个batch不能修改
	outBatch: 可以修改的batch
 */
func (db *DB) writeLocked(batch, ourBatch *Batch, merge, sync bool) error {
	// Try to flush memdb. This method would also trying to throttle writes
	// if it is too fast and compaction cannot catch-up.
	//会暂停1ms，或者关闭可写标记
	mdb, mdbFree, err := db.flush(batch.internalLen)
	if err != nil {
		db.unlockWrite(false, 0, err)
		return err
	}
	defer mdb.decref()

	var (
		overflow bool   //over flow表示从writeMergeC里读取的batch是否超过了merge limit
		merged   int   //merge的数量
		batches  = []*Batch{batch}
	)

	if merge {
		// Merge limit. 128 << 10 = 128KB
		// 1 << 20 = 1MB
		//merge limit表示可以merge其他等待写入数据的容量限制.
		var mergeLimit int  //merge limit表示可以merge的容量
		if batch.internalLen > 128<<10 {
			mergeLimit = (1 << 20) - batch.internalLen
		} else {
			mergeLimit = 128 << 10
		}
		mergeCap := mdbFree - batch.internalLen  //内存中写入batch后剩余的容量
		if mergeLimit > mergeCap {
			mergeLimit = mergeCap
		}

		//overflow时，最后一条消息没有merge进去，写入的goroutine在外面等待通知, 在这个方法里会通知unlockWrite()
		//merge成功时，每个goroutine都收到了 write merge 的ack。
	merge:
		for mergeLimit > 0 {
			select {
			case incoming := <-db.writeMergeC:
				//合并批次
				if incoming.batch != nil {
					// Merge batch.
					// 如果超出了限制，不再继续合并
					if incoming.batch.internalLen > mergeLimit {
						overflow = true
						//跳出merge标记的循环
						break merge
					}
					batches = append(batches, incoming.batch)
					mergeLimit -= incoming.batch.internalLen
				} else {
					// Merge put.
					// 合并单条记录
					internalLen := len(incoming.key) + len(incoming.value) + 8
					if internalLen > mergeLimit {
						overflow = true
						break merge
					}
					// 单条记录，创建一条空的batch处理
					if ourBatch == nil {
						ourBatch = db.batchPool.Get().(*Batch)
						ourBatch.Reset()
						batches = append(batches, ourBatch)
					}
					// We can use same batch since concurrent write doesn't
					// guarantee write order.
					ourBatch.appendRec(incoming.keyType, incoming.key, incoming.value)
					mergeLimit -= internalLen
				}
				sync = sync || incoming.sync
				merged++
				db.writeMergedC <- true //合并ack

			default:
				break merge
			}
		}
	}

	// Release ourBatch if any.
	// 不是立刻释放，这里有个defer
	if ourBatch != nil {
		defer db.batchPool.Put(ourBatch)
	}

	// Seq number.
	seq := db.seq + 1

	// Write journal.
	if err := db.writeJournal(batches, seq, sync); err != nil {
		db.unlockWrite(overflow, merged, err)  //写入失败，通知
		return err
	}

	// Put batches.
	for _, batch := range batches {
		if err := batch.putMem(seq, mdb.DB); err != nil {
			panic(err)
		}
		seq += uint64(batch.Len())
	}

	// Incr seq number.
	db.addSeq(uint64(batchesLen(batches)))

	// Rotate memdb if it's reach the threshold.
	if batch.internalLen >= mdbFree {
		db.rotateMem(0, false)
	}

	db.unlockWrite(overflow, merged, nil)
	return nil
}

// Write apply the given batch to the DB. The batch records will be applied
// sequentially. Write might be used concurrently, when used concurrently and
// batch is small enough, write will try to merge the batches. Set NoWriteMerge
// option to true to disable write merge.
//
// It is safe to modify the contents of the arguments after Write returns but
// not before. Write will not modify content of the batch.
func (db *DB) Write(batch *Batch, wo *opt.WriteOptions) error {
	if err := db.ok(); err != nil || batch == nil || batch.Len() == 0 {
		return err
	}

	// If the batch size is larger than write buffer, it may justified to write
	// using transaction instead. Using transaction the batch will be written
	// into tables directly, skipping the journaling.
	// 事务这里先跳过，回头再看
	if batch.internalLen > db.s.o.GetWriteBuffer() && !db.s.o.GetDisableLargeBatchTransaction() {
		tr, err := db.OpenTransaction()
		if err != nil {
			return err
		}
		if err := tr.Write(batch, wo); err != nil {
			tr.Discard()
			return err
		}
		return tr.Commit()
	}

	merge := !wo.GetNoWriteMerge() && !db.s.o.GetNoWriteMerge()
	sync := wo.GetSync() && !db.s.o.GetNoSync()

	// Acquire write lock.
	if merge {
		/*
		1.如果获取到锁，后面调用writeLocked独占写
		2.否则写writeMergeC表示被merge writeMergedC返回true 表示merge成功
		3.compPerErrC 表示失败
		 */
		select {
		// merge
		case db.writeMergeC <- writeMerge{sync: sync, batch: batch}:
			if <-db.writeMergedC {
				// Write is merged.
				return <-db.writeAckC
			}
		// Write is not merged, the write lock is handed to us. Continue.
		// merge没成功，db.writeMergedC返回false
		//获取到锁, 后面进行, 是如何获取到所得？
		case db.writeLockC <- struct{}{}:
		// Compaction error.
		case err := <-db.compPerErrC:
			return err
		case <-db.closeC:
			// Closed
			return ErrClosed
		}
	} else {
		select {
		// Write lock acquired.
		case db.writeLockC <- struct{}{}:
		// Compaction error.
		case err := <-db.compPerErrC:
			return err
		case <-db.closeC:
			// Closed
			return ErrClosed
		}
	}

	return db.writeLocked(batch, nil, merge, sync)
}

func (db *DB) putRec(kt keyType, key, value []byte, wo *opt.WriteOptions) error {
	if err := db.ok(); err != nil {
		return err
	}

	//是否合并，默认是合并
	merge := !wo.GetNoWriteMerge() && !db.s.o.GetNoWriteMerge()
	//是否同步写磁盘,默认是不同步写磁盘
	sync := wo.GetSync() && !db.s.o.GetNoSync()

	// Acquire write lock.
	if merge {
		select {
		case db.writeMergeC <- writeMerge{sync: sync, keyType: kt, key: key, value: value}:
			if <-db.writeMergedC { //如果merge失败， 怎么继续获取锁？
				// Write is merged.
				return <-db.writeAckC
			}
		// Write is not merged, the write lock is handed to us. Continue.
		// 锁是如何获取到得？锁怎么就传递过来了？
		// TODO
		case db.writeLockC <- struct{}{}: //获取写锁, write有一个缓冲区，如果写入成功，相当于获取到了锁
			// Write lock acquired.
		case err := <-db.compPerErrC:
			// Compaction error.
			return err
		case <-db.closeC:
			// Closed
			return ErrClosed
		}
	} else {
		select {
		case db.writeLockC <- struct{}{}:
			// Write lock acquired.
		case err := <-db.compPerErrC:
			// Compaction error.
			return err
		case <-db.closeC:
			// Closed
			return ErrClosed
		}
	}

	// batch没有合并，获取写锁，当前线上
	batch := db.batchPool.Get().(*Batch)
	batch.Reset()
	batch.appendRec(kt, key, value)
	return db.writeLocked(batch, batch, merge, sync)
}

// Put sets the value for the given key. It overwrites any previous value
// for that key; a DB is not a multi-map. Write merge also applies for Put, see
// Write.
//
// It is safe to modify the contents of the arguments after Put returns but not
// before.
func (db *DB) Put(key, value []byte, wo *opt.WriteOptions) error {
	return db.putRec(keyTypeVal, key, value, wo)
}

// Delete deletes the value for the given key. Delete will not returns error if
// key doesn't exist. Write merge also applies for Delete, see Write.
//
// It is safe to modify the contents of the arguments after Delete returns but
// not before.
func (db *DB) Delete(key []byte, wo *opt.WriteOptions) error {
	return db.putRec(keyTypeDel, key, nil, wo)
}

/*
判断mem 是否与min，max相交
 */
func isMemOverlaps(icmp *iComparer, mem *memdb.DB, min, max []byte) bool {
	iter := mem.NewIterator(nil)
	defer iter.Release()
	return (max == nil || (iter.First() && icmp.uCompare(max, internalKey(iter.Key()).ukey()) >= 0)) &&
		(min == nil || (iter.Last() && icmp.uCompare(min, internalKey(iter.Key()).ukey()) <= 0))
}

// CompactRange compacts the underlying DB for the given key range.
// In particular, deleted and overwritten versions are discarded,
// and the data is rearranged to reduce the cost of operations
// needed to access the data. This operation should typically only
// be invoked by users who understand the underlying implementation.
//
// A nil Range.Start is treated as a key before all keys in the DB.
// And a nil Range.Limit is treated as a key after all keys in the DB.
// Therefore if both is nil then it will compact entire DB.
/*
压缩给定key范围的数据
删除或者被覆盖的版本会被删除.
 */
func (db *DB) CompactRange(r util.Range) error {
	if err := db.ok(); err != nil {
		return err
	}

	// Lock writer.
	select {
	case db.writeLockC <- struct{}{}:
	case err := <-db.compPerErrC:
		return err
	case <-db.closeC:
		return ErrClosed
	}

	// Check for overlaps in memdb.
	mdb := db.getEffectiveMem()
	if mdb == nil {
		return ErrClosed
	}
	defer mdb.decref()
	if isMemOverlaps(db.s.icmp, mdb.DB, r.Start, r.Limit) {
		// Memdb compaction.
		if _, err := db.rotateMem(0, false); err != nil {
			<-db.writeLockC
			return err
		}
		<-db.writeLockC
		if err := db.compTriggerWait(db.mcompCmdC); err != nil {
			return err
		}
	} else {
		<-db.writeLockC
	}

	// Table compaction.
	return db.compTriggerRange(db.tcompCmdC, -1, r.Start, r.Limit)
}

// SetReadOnly makes DB read-only. It will stay read-only until reopened.
func (db *DB) SetReadOnly() error {
	if err := db.ok(); err != nil {
		return err
	}

	// Lock writer.
	select {
	case db.writeLockC <- struct{}{}:
		db.compWriteLocking = true
	case err := <-db.compPerErrC:
		return err
	case <-db.closeC:
		return ErrClosed
	}

	// Set compaction read-only.
	select {
	case db.compErrSetC <- ErrReadOnly:
	case perr := <-db.compPerErrC:
		return perr
	case <-db.closeC:
		return ErrClosed
	}

	return nil
}
