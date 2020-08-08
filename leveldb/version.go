// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

type tSet struct {
	level int
	table *tFile
}

type version struct {
	id int64 // unique monotonous increasing version id
	s  *session

	levels []tFiles // 每一层对应的tFiles

	// Level that should be compacted next and its compaction score.
	// Score < 1 means compaction is not strictly needed. These fields
	// are initialized by computeCompaction()
	// compaction level, 下一个需要compaction的level和它的分数，
	// 分数小于1，意味着压缩不是必须的
	cLevel int
	cScore float64

	cSeek unsafe.Pointer

	closing  bool
	ref      int
	released bool
}

// newVersion creates a new version with an unique monotonous increasing id.
// 什么时候创建version？
// 创建session的时候会创建version, 同一个session递增
func newVersion(s *session) *version {
	id := atomic.AddInt64(&s.ntVersionId, 1)
	nv := &version{s: s, id: id - 1}
	return nv
}

//增加引用计数
func (v *version) incref() {
	if v.released {
		panic("already released")
	}

	//第一次被引用,传递vTask到session
	v.ref++
	if v.ref == 1 {
		select {
		case v.s.refCh <- &vTask{vid: v.id, files: v.levels, created: time.Now()}:
			// We can use v.levels directly here since it is immutable.
		case <-v.s.closeC:
			v.s.log("reference loop already exist")
		}
	}
}

//释放版本
func (v *version) releaseNB() {
	v.ref--
	if v.ref > 0 {
		return
	} else if v.ref < 0 {
		panic("negative version ref")
	}
	select {
	case v.s.relCh <- &vTask{vid: v.id, files: v.levels, created: time.Now()}:
		// We can use v.levels directly here since it is immutable.
	case <-v.s.closeC:
		v.s.log("reference loop already exist")
	}

	v.released = true
}

func (v *version) release() {
	v.s.vmu.Lock()
	v.releaseNB()
	v.s.vmu.Unlock()
}

/**
	遍历tFiles，对于满足条件的tFile调用f(level int, t * tFile) bool
	f : 对于每个需要遍历的文件，调用f. f返回true表示需要继续迭代，返回false表示不需要继续迭代
	lf : 找到了(f 返回true)，调用lf方法.可以用于处理0层的情况，查找元素的时候，0层需要遍历所有的文件。
 */
func (v *version) walkOverlapping(aux tFiles, ikey internalKey, f func(level int, t *tFile) bool, lf func(level int) bool) {
	ukey := ikey.ukey()

	// Aux level.
	if aux != nil {
		for _, t := range aux {
			if t.overlaps(v.s.icmp, ukey, ukey) {
				if !f(-1, t) {
					return
				}
			}
		}

		if lf != nil && !lf(-1) {
			return
		}
	}

	// Walk tables level-by-level.
	for level, tables := range v.levels {
		if len(tables) == 0 {
			continue
		}

		if level == 0 {
			// 0层的文件可能互相覆盖，需要全部遍历
			// Level-0 files may overlap each other. Find all files that
			// overlap ukey.
			for _, t := range tables {
				if t.overlaps(v.s.icmp, ukey, ukey) {
					//不需要继续迭代,直接返回
					if !f(level, t) {
						return
					}
				}
			}
		} else {
			if i := tables.searchMax(v.s.icmp, ikey); i < len(tables) {
				t := tables[i]
				if v.s.icmp.uCompare(ukey, t.imin.ukey()) >= 0 {
					//找到，不需要继续迭代
					if !f(level, t) {
						return
					}
				}
			}
		}

		//for循环内部，对于0层，如果存在一个或者多个key则认为已经找到，调用lf逻辑
		if lf != nil && !lf(level) {
			return
		}
	}
}

/*
从SSTable读取key
noValue: 用于标记是否需要返回值

返回值：
value : 值
tcomp :
err :
 */
func (v *version) get(aux tFiles, ikey internalKey, ro *opt.ReadOptions, noValue bool) (value []byte, tcomp bool, err error) {
	if v.closing {
		return nil, false, ErrClosed
	}

	ukey := ikey.ukey()
	sampleSeeks := !v.s.o.GetDisableSeeksCompaction()

	var (
		tset  *tSet
		tseek bool

		// Level-0.
		// level-0的key可能会overlap，这里保存一些level0之前的信息
		zfound bool
		zseq   uint64  //保存seq最新的那个
		zkt    keyType //对应的keyType
		zval   []byte //对应的value
	)

	err = ErrNotFound

	// Since entries never hop across level, finding key/value
	// in smaller level make later levels irrelevant.
	v.walkOverlapping(aux, ikey, func(level int, t *tFile) bool {
		if sampleSeeks && level >= 0 && !tseek {
			if tset == nil {
				tset = &tSet{level, t}
			} else {
				tseek = true
			}
		}

		var (
			fikey, fval []byte
			ferr        error
		)
		//是否返回value
		if noValue {
			fikey, ferr = v.s.tops.findKey(t, ikey, ro)
		} else {
			fikey, fval, ferr = v.s.tops.find(t, ikey, ro)
		}

		//出现错误，返回
		switch ferr {
		case nil:
		case ErrNotFound:
			return true
		default:
			err = ferr
			return false
		}

		//fukey: 从文件中找到的第一个大于等于ukey的
		//如果sstable里的fukey与要查找的ikey里的ukey相同，但是ikey里的seq number是当前的sequence number，比sstable里的seq要大
		//这样不会导致文件里的ikey小于fikey吗？
		//ANSWER： internalKey比较时，sequence number更小的那个值更大
		if fukey, fseq, fkt, fkerr := parseInternalKey(fikey); fkerr == nil {
			if v.s.icmp.uCompare(ukey, fukey) == 0 {
				// Level <= 0 may overlaps each-other.
				// 如果是0层，找到这个key也不一定是最新的，保存信息，继续遍历其他文件
				if level <= 0 {
					if fseq >= zseq {
						zfound = true
						zseq = fseq
						zkt = fkt
						zval = fval
					}
				} else {
					switch fkt {
					case keyTypeVal:
						value = fval
						err = nil
					case keyTypeDel:
					default:
						panic("leveldb: invalid internalKey type")
					}
					//return false说明已经找到key， 不需要继续迭代了
					return false
				}
			}
		} else {
			//发生了错误，也不再继续迭代
			err = fkerr
			return false
		}

		return true
		//lf参数，针对0层的情况
	}, func(level int) bool {
		if zfound {
			switch zkt {
			case keyTypeVal:
				value = zval
				err = nil
			case keyTypeDel:
			default:
				panic("leveldb: invalid internalKey type")
			}
			//如果找到，不需要继续遍历。
			return false
		}

		return true
	})

	if tseek && tset.table.consumeSeek() <= 0 {
		tcomp = atomic.CompareAndSwapPointer(&v.cSeek, nil, unsafe.Pointer(tset))
	}
	return
}

//抽样读取
//没看明白
func (v *version) sampleSeek(ikey internalKey) (tcomp bool) {
	var tset *tSet

	v.walkOverlapping(nil, ikey, func(level int, t *tFile) bool {
		if tset == nil {
			tset = &tSet{level, t}
			return true
		}
		if tset.table.consumeSeek() <= 0 {
			tcomp = atomic.CompareAndSwapPointer(&v.cSeek, nil, unsafe.Pointer(tset))
		}
		return false
	}, nil)

	return
}

/*
根据version里的sstable返回迭代器
 */
func (v *version) getIterators(slice *util.Range, ro *opt.ReadOptions) (its []iterator.Iterator) {
	strict := opt.GetStrict(v.s.o.Options, ro, opt.StrictReader)
	for level, tables := range v.levels {
		if level == 0 {
			// Merge all level zero files together since they may overlap.
			for _, t := range tables {
				its = append(its, v.s.tops.newIterator(t, slice, ro))
			}
		} else if len(tables) != 0 {
			its = append(its, iterator.NewIndexedIterator(tables.newIndexIterator(v.s.tops, v.s.icmp, slice, ro), strict))
		}
	}
	return
}

func (v *version) newStaging() *versionStaging {
	return &versionStaging{base: v}
}

// Spawn a new version based on this version.
// 根据version v和变更记录产生一个新的version
func (v *version) spawn(r *sessionRecord, trivial bool) *version {
	staging := v.newStaging()
	staging.commit(r)
	return staging.finish(trivial)
}

func (v *version) fillRecord(r *sessionRecord) {
	for level, tables := range v.levels {
		for _, t := range tables {
			r.addTableFile(level, t)
		}
	}
}

func (v *version) tLen(level int) int {
	if level < len(v.levels) {
		return len(v.levels[level])
	}
	return 0
}

func (v *version) offsetOf(ikey internalKey) (n int64, err error) {
	for level, tables := range v.levels {
		for _, t := range tables {
			if v.s.icmp.Compare(t.imax, ikey) <= 0 {
				// Entire file is before "ikey", so just add the file size
				n += t.size
			} else if v.s.icmp.Compare(t.imin, ikey) > 0 {
				// Entire file is after "ikey", so ignore
				if level > 0 {
					// Files other than level 0 are sorted by meta->min, so
					// no further files in this level will contain data for
					// "ikey".
					break
				}
			} else {
				// "ikey" falls in the range for this table. Add the
				// approximate offset of "ikey" within the table.
				if m, err := v.s.tops.offsetOf(t, ikey); err == nil {
					n += m
				} else {
					return 0, err
				}
			}
		}
	}

	return
}

func (v *version) pickMemdbLevel(umin, umax []byte, maxLevel int) (level int) {
	if maxLevel > 0 {
		if len(v.levels) == 0 {
			return maxLevel
		}
		if !v.levels[0].overlaps(v.s.icmp, umin, umax, true) {
			var overlaps tFiles
			for ; level < maxLevel; level++ {
				if pLevel := level + 1; pLevel >= len(v.levels) {
					return maxLevel
				} else if v.levels[pLevel].overlaps(v.s.icmp, umin, umax, false) {
					break
				}
				if gpLevel := level + 2; gpLevel < len(v.levels) {
					overlaps = v.levels[gpLevel].getOverlaps(overlaps, v.s.icmp, umin, umax, false)
					if overlaps.size() > int64(v.s.o.GetCompactionGPOverlaps(level)) {
						break
					}
				}
			}
		}
	}
	return
}

// 计算分数
func (v *version) computeCompaction() {
	// Precomputed best level for next compaction
	bestLevel := int(-1)
	bestScore := float64(-1)

	statFiles := make([]int, len(v.levels))
	statSizes := make([]string, len(v.levels))
	statScore := make([]string, len(v.levels))
	statTotSize := int64(0)

	for level, tables := range v.levels {
		var score float64
		size := tables.size()
		if level == 0 {
			// We treat level-0 specially by bounding the number of files
			// instead of number of bytes for two reasons:
			//
			// (1) With larger write-buffer sizes, it is nice not to do too
			// many level-0 compaction.
			//
			// (2) The files in level-0 are merged on every read and
			// therefore we wish to avoid too many files when the individual
			// file size is small (perhaps because of a small write-buffer
			// setting, or very high compression ratios, or lots of
			// overwrites/deletions).
			score = float64(len(tables)) / float64(v.s.o.GetCompactionL0Trigger())
		} else {
			score = float64(size) / float64(v.s.o.GetCompactionTotalSize(level))
		}

		if score > bestScore {
			bestLevel = level
			bestScore = score
		}

		statFiles[level] = len(tables)
		statSizes[level] = shortenb(int(size))
		statScore[level] = fmt.Sprintf("%.2f", score)
		statTotSize += size
	}

	v.cLevel = bestLevel
	v.cScore = bestScore

	v.s.logf("version@stat F·%v S·%s%v Sc·%v", statFiles, shortenb(int(statTotSize)), statSizes, statScore)
}

//是否需要压缩
func (v *version) needCompaction() bool {
	return v.cScore >= 1 || atomic.LoadPointer(&v.cSeek) != nil
}

//
type tablesScratch struct {
	added   map[int64]atRecord
	deleted map[int64]struct{}
}

type versionStaging struct {
	base   *version
	//每个level的 添加、删除信息
	levels []tablesScratch
}

//获取第level层的scratch信息
func (p *versionStaging) getScratch(level int) *tablesScratch {
	if level >= len(p.levels) {
		newLevels := make([]tablesScratch, level+1)
		copy(newLevels, p.levels)
		p.levels = newLevels
	}
	return &(p.levels[level])
}

func (p *versionStaging) commit(r *sessionRecord) {
	// Deleted tables.
	//处理删除的文件信息:1.标记文件被删除  2.从文件添加集合中删除
	//r 是每一层删除的文件
	for _, r := range r.deletedTables {
		scratch := p.getScratch(r.level)
		if r.level < len(p.base.levels) && len(p.base.levels[r.level]) > 0 {
			if scratch.deleted == nil {
				scratch.deleted = make(map[int64]struct{})
			}
			scratch.deleted[r.num] = struct{}{}
		}
		if scratch.added != nil {
			delete(scratch.added, r.num)
		}
	}

	// New tables.
	for _, r := range r.addedTables {
		scratch := p.getScratch(r.level)
		if scratch.added == nil {
			scratch.added = make(map[int64]atRecord)
		}
		scratch.added[r.num] = r
		if scratch.deleted != nil {
			delete(scratch.deleted, r.num)
		}
	}
}

/*

 */
func (p *versionStaging) finish(trivial bool) *version {
	// Build new version.
	nv := newVersion(p.base.s)
	numLevel := len(p.levels)
	if len(p.base.levels) > numLevel {
		numLevel = len(p.base.levels)
	}
	nv.levels = make([]tFiles, numLevel)
	for level := 0; level < numLevel; level++ {
		var baseTabels tFiles
		if level < len(p.base.levels) {
			baseTabels = p.base.levels[level]
		}

		if level < len(p.levels) {
			scratch := p.levels[level]

			// Short circuit if there is no change at all.
			if len(scratch.added) == 0 && len(scratch.deleted) == 0 {
				nv.levels[level] = baseTabels
				continue
			}

			var nt tFiles
			// Prealloc list if possible.
			if n := len(baseTabels) + len(scratch.added) - len(scratch.deleted); n > 0 {
				nt = make(tFiles, 0, n)
			}

			// Base tables.
			for _, t := range baseTabels {
				if _, ok := scratch.deleted[t.fd.Num]; ok {
					continue
				}
				if _, ok := scratch.added[t.fd.Num]; ok {
					continue
				}
				nt = append(nt, t)
			}

			// Avoid resort if only files in this level are deleted
			if len(scratch.added) == 0 {
				nv.levels[level] = nt
				continue
			}

			// For normal table compaction, one compaction will only involve two levels
			// of files. And the new files generated after merging the source level and
			// source+1 level related files can be inserted as a whole into source+1 level
			// without any overlap with the other source+1 files.
			//
			// When the amount of data maintained by leveldb is large, the number of files
			// per level will be very large. While qsort is very inefficient for sorting
			// already ordered arrays. Therefore, for the normal table compaction, we use
			// binary search here to find the insert index to insert a batch of new added
			// files directly instead of using qsort.
			if trivial && len(scratch.added) > 0 {
				added := make(tFiles, 0, len(scratch.added))
				for _, r := range scratch.added {
					added = append(added, tableFileFromRecord(r))
				}
				if level == 0 {
					added.sortByNum()
					index := nt.searchNumLess(added[len(added)-1].fd.Num)
					nt = append(nt[:index], append(added, nt[index:]...)...)
				} else {
					added.sortByKey(p.base.s.icmp)
					_, amax := added.getRange(p.base.s.icmp)
					index := nt.searchMin(p.base.s.icmp, amax)
					nt = append(nt[:index], append(added, nt[index:]...)...)
				}
				nv.levels[level] = nt
				continue
			}

			// New tables.
			for _, r := range scratch.added {
				nt = append(nt, tableFileFromRecord(r))
			}

			if len(nt) != 0 {
				// Sort tables.
				if level == 0 {
					nt.sortByNum()
				} else {
					nt.sortByKey(p.base.s.icmp)
				}

				nv.levels[level] = nt
			}
		} else {
			nv.levels[level] = baseTabels
		}
	}

	// Trim levels.
	n := len(nv.levels)
	for ; n > 0 && nv.levels[n-1] == nil; n-- {
	}
	nv.levels = nv.levels[:n]

	// Compute compaction score for new version.
	nv.computeCompaction()

	return nv
}

type versionReleaser struct {
	v    *version
	once bool
}

func (vr *versionReleaser) Release() {
	v := vr.v
	v.s.vmu.Lock()
	if !vr.once {
		v.releaseNB()
		vr.once = true
	}
	v.s.vmu.Unlock()
}
