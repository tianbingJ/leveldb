// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package iterator

import (
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// IteratorIndexer is the interface that wraps CommonIterator and basic Get
// method. IteratorIndexer provides index for indexed iterator.
/*
	索引迭代器
 */
type IteratorIndexer interface {
	CommonIterator

	// Get returns a new data iterator for the current position, or nil if
	// done.
	// 返回一个数据迭代器
	Get() Iterator
}

type indexedIterator struct {
	util.BasicReleaser
	index  IteratorIndexer
	//strict?
	strict bool

	// 数据迭代器
	data   Iterator
	err    error
	// err发生时的回调函数
	errf   func(err error)
	closed bool
}

// 使用当前索引返回一个数据迭代器
func (i *indexedIterator) setData() {
	if i.data != nil {
		i.data.Release()
	}
	i.data = i.index.Get()
}

// 清楚数据
func (i *indexedIterator) clearData() {
	if i.data != nil {
		i.data.Release()
	}
	i.data = nil
}

//
func (i *indexedIterator) indexErr() {
	if err := i.index.Error(); err != nil {
		if i.errf != nil {
			i.errf(err)
		}
		i.err = err
	}
}

func (i *indexedIterator) dataErr() bool {
	if err := i.data.Error(); err != nil {
		if i.errf != nil {
			i.errf(err)
		}
		if i.strict || !errors.IsCorrupted(err) {
			i.err = err
			return true
		}
	}
	return false
}

// 是否合法为什么是判断data ?
func (i *indexedIterator) Valid() bool {
	return i.data != nil && i.data.Valid()
}

func (i *indexedIterator) First() bool {
	if i.err != nil {
		return false
	} else if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if !i.index.First() {
		i.indexErr()
		i.clearData()
		return false
	}
	i.setData()
	return i.Next()
}

func (i *indexedIterator) Last() bool {
	if i.err != nil {
		return false
	} else if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if !i.index.Last() {
		i.indexErr()
		i.clearData()
		return false
	}
	i.setData()
	if !i.data.Last() {
		if i.dataErr() {
			return false
		}
		i.clearData()
		return i.Prev()
	}
	return true
}

func (i *indexedIterator) Seek(key []byte) bool {
	if i.err != nil {
		return false
	} else if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if !i.index.Seek(key) {
		i.indexErr()
		i.clearData()
		return false
	}
	i.setData()
	if !i.data.Seek(key) {
		if i.dataErr() {
			return false
		}
		i.clearData()
		return i.Next()
	}
	return true
}

// 索引迭代器Next方法
//
func (i *indexedIterator) Next() bool {
	if i.err != nil {
		return false
	} else if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	switch {
	case i.data != nil && !i.data.Next():
		if i.dataErr() {
			return false
		}
		i.clearData()
		fallthrough
	case i.data == nil:
		if !i.index.Next() {
			i.indexErr()
			return false
		}
		i.setData()
		return i.Next()
	}
	return true
}

func (i *indexedIterator) Prev() bool {
	if i.err != nil {
		return false
	} else if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	switch {
	case i.data != nil && !i.data.Prev():
		if i.dataErr() {
			return false
		}
		i.clearData()
		fallthrough
	case i.data == nil:
		if !i.index.Prev() {
			i.indexErr()
			return false
		}
		i.setData()
		if !i.data.Last() {
			if i.dataErr() {
				return false
			}
			i.clearData()
			return i.Prev()
		}
	}
	return true
}

func (i *indexedIterator) Key() []byte {
	if i.data == nil {
		return nil
	}
	return i.data.Key()
}

func (i *indexedIterator) Value() []byte {
	if i.data == nil {
		return nil
	}
	return i.data.Value()
}

func (i *indexedIterator) Release() {
	i.clearData()
	i.index.Release()
	i.BasicReleaser.Release()
}

func (i *indexedIterator) Error() error {
	if i.err != nil {
		return i.err
	}
	if err := i.index.Error(); err != nil {
		return err
	}
	return nil
}

func (i *indexedIterator) SetErrorCallback(f func(err error)) {
	i.errf = f
}

// NewIndexedIterator returns an 'indexed iterator'. An index is iterator
// that returns another iterator, a 'data iterator'. A 'data iterator' is the
// iterator that contains actual key/value pairs.
//
// If strict is true the any 'corruption errors' (i.e errors.IsCorrupted(err) == true)
// won't be ignored and will halt 'indexed iterator', otherwise the iterator will
// continue to the next 'data iterator'. Corruption on 'index iterator' will not be
// ignored and will halt the iterator.
func NewIndexedIterator(index IteratorIndexer, strict bool) Iterator {
	return &indexedIterator{index: index, strict: strict}
}
