// Copyright (c) 2014, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package iterator

import (
	"github.com/syndtr/goleveldb/leveldb/util"
)

// BasicArray is the interface that wraps basic Len and Search method.
type BasicArray interface {
	// Len returns length of the array.
	Len() int

	// Search finds smallest index that point to a key that is greater
	// than or equal to the given key.
	//找到第一个大于等于只等key的下标
	//数组需是有序数组?
	Search(key []byte) int
}

// Array is the interface that wraps BasicArray and basic Index method.
type Array interface {
	BasicArray

	// Index returns key/value pair with index of i.
	// 返回下标是i的key value
	Index(i int) (key, value []byte)
}

// Array is the interface that wraps BasicArray and basic Get method.
//
type ArrayIndexer interface {
	BasicArray

	// Get returns a new data iterator with index of i.
	Get(i int) Iterator
}

// 数组迭代器
type basicArrayIterator struct {
	util.BasicReleaser
	array BasicArray
	pos   int
	err   error
}

func (i *basicArrayIterator) Valid() bool {
	return i.pos >= 0 && i.pos < i.array.Len() && !i.Released()
}

// 设置迭代器执行第一个元素
func (i *basicArrayIterator) First() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if i.array.Len() == 0 {
		i.pos = -1
		return false
	}
	i.pos = 0
	return true
}

func (i *basicArrayIterator) Last() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	n := i.array.Len()
	if n == 0 {
		i.pos = 0
		return false
	}
	i.pos = n - 1
	return true
}

//定位到第一个大于等于key的位置
func (i *basicArrayIterator) Seek(key []byte) bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	n := i.array.Len()
	if n == 0 {
		i.pos = 0
		return false
	}
	i.pos = i.array.Search(key)
	if i.pos >= n {
		return false
	}
	return true
}

//往后移动一个位置
func (i *basicArrayIterator) Next() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	i.pos++
	if n := i.array.Len(); i.pos >= n {
		i.pos = n
		return false
	}
	return true
}

//往前移动一个位置
func (i *basicArrayIterator) Prev() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	i.pos--
	if i.pos < 0 {
		i.pos = -1
		return false
	}
	return true
}

func (i *basicArrayIterator) Error() error { return i.err }

type arrayIterator struct {
	basicArrayIterator
	array      Array
	//这个pos和basicArrayIterator中的pos有何不一样？

	pos        int
	key, value []byte
}

//用basicArrayIterator中的pos指向的key value更新当前缓存的key value
func (i *arrayIterator) updateKV() {
	if i.pos == i.basicArrayIterator.pos {
		return
	}
	i.pos = i.basicArrayIterator.pos
	if i.Valid() {
		i.key, i.value = i.array.Index(i.pos)
	} else {
		i.key = nil
		i.value = nil
	}
}

func (i *arrayIterator) Key() []byte {
	i.updateKV()
	return i.key
}

func (i *arrayIterator) Value() []byte {
	i.updateKV()
	return i.value
}

type arrayIteratorIndexer struct {
	basicArrayIterator
	array ArrayIndexer
}

func (i *arrayIteratorIndexer) Get() Iterator {
	if i.Valid() {
		return i.array.Get(i.basicArrayIterator.pos)
	}
	return nil
}

// NewArrayIterator returns an iterator from the given array.
func NewArrayIterator(array Array) Iterator {
	return &arrayIterator{
		basicArrayIterator: basicArrayIterator{array: array, pos: -1},
		array:              array,
		pos:                -1,
	}
}

// NewArrayIndexer returns an index iterator from the given array.
func NewArrayIndexer(array ArrayIndexer) IteratorIndexer {
	return &arrayIteratorIndexer{
		basicArrayIterator: basicArrayIterator{array: array, pos: -1},
		array:              array,
	}
}
