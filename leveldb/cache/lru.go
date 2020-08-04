// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package cache

import (
	"sync"
	"unsafe"
)

/*
lru与cache配合，有几种数据结构：

lru : lru链表
lruNode : lru链表里的节点
Node  :  cache中hash表中的节点
CacheData : Node中的数据，只想在lru中的lruNode，可以从cache中定位到lru中的节点。
Handle : 含有Node的指针，并提供方法:
	1.Value(): 得到指向的Node中的数据
	2.Release() : 减少指向cache Node的引用
*/

type lruNode struct {
	n   *Node
	h   *Handle
	/*
	将hash表对应的lruNode节点从链表中删除，并"尝试"从hash表中删除（当引用计数为0时才真正删除）。
	 */
	ban bool

	next, prev *lruNode
}

//节点n插入在at节点的后面
func (n *lruNode) insert(at *lruNode) {
	x := at.next
	at.next = n
	n.prev = at
	n.next = x
	x.prev = n
}

//移除node n
func (n *lruNode) remove() {
	if n.prev != nil {
		n.prev.next = n.next
		n.next.prev = n.prev
		n.prev = nil
		n.next = nil
	} else {
		panic("BUG: removing removed node")
	}
}

type lru struct {
	mu       sync.Mutex
	capacity int
	used     int
	recent   lruNode
}

func (r *lru) reset() {
	r.recent.next = &r.recent
	r.recent.prev = &r.recent
	r.used = 0
}

func (r *lru) Capacity() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capacity
}

//设置容量, 从lru list后面删除更旧的数据
func (r *lru) SetCapacity(capacity int) {
	var evicted []*lruNode

	r.mu.Lock()
	r.capacity = capacity
	for r.used > r.capacity {
		rn := r.recent.prev
		if rn == nil {
			panic("BUG: invalid LRU used or capacity counter")
		}
		rn.remove()
		rn.n.CacheData = nil
		r.used -= rn.n.Size()
		evicted = append(evicted, rn)
	}
	r.mu.Unlock()

	for _, rn := range evicted {
		rn.h.Release()
	}
}

/*
将Node提升为lru中的头结点
Node可能之前并不是lru中的节点(case 1)，把Node放入lru中的过程，可能会导致lru的空间超过capacity，需要驱逐部分节点。
*/
func (r *lru) Promote(n *Node) {
	var evicted []*lruNode

	r.mu.Lock()
	//case 1: 之前没有在LRU链表之中
	if n.CacheData == nil {
		//Node n的大小要小于lru的容量，否则直接忽略
		if n.Size() <= r.capacity {
			rn := &lruNode{n: n, h: n.GetHandle()}
			rn.insert(&r.recent)
			n.CacheData = unsafe.Pointer(rn)
			r.used += n.Size()

			//容量超过了lru的容量，需要从lru中驱逐一些节点
			for r.used > r.capacity {
				rn := r.recent.prev
				if rn == nil {
					panic("BUG: invalid LRU used or capacity counter")
				}
				rn.remove()
				rn.n.CacheData = nil  //设置Node指向的lru node是nil
				r.used -= rn.n.Size()
				evicted = append(evicted, rn)
			}
		}
	} else {
		rn := (*lruNode)(n.CacheData)
		//如果lru没有被ban， 则插入到链表头
		if !rn.ban {
			rn.remove()
			rn.insert(&r.recent)
		}
	}
	r.mu.Unlock()

	for _, rn := range evicted {
		rn.h.Release()
	}
}

//将节点从lru中删除，ban掉.
//与此同时并不释放lruNode的空间,Node中还存在着指向lru中的节点.
//为什么不设置 n.CacheData = nil ?
func (r *lru) Ban(n *Node) {
	r.mu.Lock()
	if n.CacheData == nil {
		n.CacheData = unsafe.Pointer(&lruNode{n: n, ban: true})
	} else {
		rn := (*lruNode)(n.CacheData)
		if !rn.ban {
			rn.remove()
			rn.ban = true
			r.used -= rn.n.Size()
			r.mu.Unlock()

			rn.h.Release()
			rn.h = nil
			return
		}
	}
	r.mu.Unlock()
}

/*
Evict只是接触Node和lruNode的关系？ 并没有从lru中删除lruNode节点? 为什么不调用rn.remove()
 */
func (r *lru) Evict(n *Node) {
	r.mu.Lock()
	rn := (*lruNode)(n.CacheData)
	if rn == nil || rn.ban {
		r.mu.Unlock()
		return
	}
	//Node不再指向lruNode
	n.CacheData = nil
	r.mu.Unlock()
	rn.h.Release()
}

/*
evict 这个namespace 所有的node.
针对lru中的Node会调用remove()方法从lru中删除。为什么Evict()方法不会删除？
 */
func (r *lru) EvictNS(ns uint64) {
	var evicted []*lruNode

	r.mu.Lock()
	for e := r.recent.prev; e != &r.recent; {
		rn := e
		e = e.prev
		if rn.n.NS() == ns {
			rn.remove()
			rn.n.CacheData = nil
			r.used -= rn.n.Size()
			evicted = append(evicted, rn)
		}
	}
	r.mu.Unlock()

	for _, rn := range evicted {
		rn.h.Release()
	}
}

func (r *lru) EvictAll() {
	r.mu.Lock()
	back := r.recent.prev
	for rn := back; rn != &r.recent; rn = rn.prev {
		rn.n.CacheData = nil
	}
	r.reset()
	r.mu.Unlock()

	for rn := back; rn != &r.recent; rn = rn.prev {
		rn.h.Release()
	}
}

func (r *lru) Close() error {
	return nil
}

// NewLRU create a new LRU-cache.
func NewLRU(capacity int) Cacher {
	r := &lru{capacity: capacity}
	r.reset()
	return r
}
