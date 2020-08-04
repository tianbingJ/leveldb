// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package comparer provides interface and implementation for ordering
// sets of data.
/*
比较器，定义比较的接口
 */
package comparer

// BasicComparer is the interface that wraps the basic Compare method.
//BaseicComparer实现比较方法，返回 -1 0 +1
type BasicComparer interface {
	// Compare returns -1, 0, or +1 depending on whether a is 'less than',
	// 'equal to' or 'greater than' b. The two arguments can only be 'equal'
	// if their contents are exactly equal. Furthermore, the empty slice
	// must be 'less than' any non-empty slice.
	Compare(a, b []byte) int
}

// Comparer defines a total ordering over the space of []byte keys: a 'less
// than' relationship.
type Comparer interface {
	BasicComparer

	// Name returns name of the comparer.
	//
	// The Level-DB on-disk format stores the comparer name, and opening a
	// database with a different comparer from the one it was created with
	// will result in an error.
	//
	// An implementation to a new name whenever the comparer implementation
	// changes in a way that will cause the relative ordering of any two keys
	// to change.
	//
	// Names starting with "leveldb." are reserved and should not be used
	// by any users of this package.
	//返回比较器的名字
	//leveldb会存储这个名字，用于之前不同名字打开一个db会产生错误。
	Name() string

	// Bellow are advanced functions used to reduce the space requirements
	// for internal data structures such as index blocks.

	// Separator appends a sequence of bytes x to dst such that a <= x && x < b,
	// where 'less than' is consistent with Compare. An implementation should
	// return nil if x equal to a.
	//
	// Either contents of a or b should not by any means modified. Doing so
	// may cause corruption on the internal state.
	// 用于减少内部数据结构的空间，比如indexBlocks
	// 构造一个能区分a和b的字节列表，满足 a < x < b, 并添加到dst中。
	// 如果能构造成功，返回添加后的dst，否则返回nil.
	Separator(dst, a, b []byte) []byte

	// Successor appends a sequence of bytes x to dst such that x >= b, where
	// 'less than' is consistent with Compare. An implementation should return
	// nil if x equal to b.
	//
	// Contents of b should not by any means modified. Doing so may cause
	// corruption on the internal state.
	// 利用b构造一个比b大的字节列表，如果构造不成功，返回nil
	Successor(dst, b []byte) []byte
}
