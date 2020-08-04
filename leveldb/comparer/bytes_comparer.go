// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package comparer

import "bytes"

type bytesComparer struct{}

func (bytesComparer) Compare(a, b []byte) int {
	return bytes.Compare(a, b)
}

func (bytesComparer) Name() string {
	return "leveldb.BytewiseComparator"
}

//如果a和b某个字节序列是另外一个字节序列的前缀，则不做处理。
//否则，判断是否满足条件: a <= x < b.
//	x是根据a构造，a中第一个与b不同的字节 + 1， 如果能满足上述条件，则把x添加到dst中
func (bytesComparer) Separator(dst, a, b []byte) []byte {
	//n取a和b长度的最小值
	i, n := 0, len(a)
	if n > len(b) {
		n = len(b)
	}

	for ; i < n && a[i] == b[i]; i++ {
	}
	if i >= n {
		// Do not shorten if one string is a prefix of the other
	} else if c := a[i]; c < 0xff && c+1 < b[i] {
		//c < 0xff是为了防止c + 1溢出
		dst = append(dst, a[:i+1]...)
		dst[len(dst)-1]++
		return dst
	}
	return nil
}


/*
构造一个比大字节列表，添加到dst中.
构造方法：遍历b中字节列表，只要不等于0xff，则加1.
如果b中全是0xff，返回nil，不作处理
 */
func (bytesComparer) Successor(dst, b []byte) []byte {
	for i, c := range b {
		if c != 0xff {
			dst = append(dst, b[:i+1]...)
			dst[len(dst)-1]++
			return dst
		}
	}
	return nil
}

// DefaultComparer are default implementation of the Comparer interface.
// It uses the natural ordering, consistent with bytes.Compare.
var DefaultComparer = bytesComparer{}
