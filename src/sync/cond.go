// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond implements a condition variable, a rendezvous point
// for goroutines waiting for or announcing the occurrence
// of an event.
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and
// when calling the Wait method.
//
// A Cond must not be copied after first use.
type Cond struct {
	// noCopy可以潜入到结构中，在第一次使用后不可复制，使用go vet作为检测使用
	noCopy noCopy

	// L is held while observing or changing the condition
	// 根据需求初始化不同的锁, 如*Mutex和*RWMutex
	L Locker

	// 通知列表，调用Wait方法的goroutine会被放入list中，每次唤醒，从这里取出
	notify  notifyList

	// 复制检查，检查cond实例是否被复制
	checker copyChecker
}

// NewCond returns a new Cond with Locker l.
// Cond不是开箱即用的，需要调用NewCond方法初始化一个
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution
// of the calling goroutine. After later resuming execution,
// Wait locks c.L before returning. Unlike in other systems,
// Wait cannot return unless awoken by Broadcast or Signal.
//
// Because c.L is not locked when Wait first resumes, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
//
// 	  // 先上锁，表示当前goroutine持有此资源，其他goroutine不能访问此资源
//    c.L.Lock()
//    // 这里用for可以不停的检查条件的状态，因为每次唤醒，并不意味着条件满足，不满足的情况下继续Wait
// 	  // 优先建议用Broadcast通知，如果用Signal通知，可能导致队首的goroutine一直不满足，然后知道wait，
//    for !condition() {
//        c.Wait()
//    }
// 	  // 满足条件后处理业务，处理完成后，释放对资源的持有，让其他goroutine可以工作
//    ... make use of condition ...
//    c.L.Unlock()
//
func (c *Cond) Wait() {
	// 检查c是否被复制，如果是就panic
	c.checker.check()
	// 条件不满足的时候调用Wait方法，把当前goroutine放到notify的队尾
	t := runtime_notifyListAdd(&c.notify)
	// 然后解锁对资源的持有，让其他goroutine使用（比如说，生产者队列满了，要让消费者获取锁并消费队列，并通知生产者）
	c.L.Unlock()
	// 等待唤醒操作（Signal/Broadcast）
	runtime_notifyListWait(&c.notify, t)
	// 唤醒后，持有资源的锁，再次判断是否满足条件
	c.L.Lock()
}

// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Signal() {
	c.checker.check()
	// 通知等待中goroutine，取notify队列中的第一个（队首）
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Broadcast() {
	c.checker.check()
	// 通知所有等待中的goroutine
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr

func (c *copyChecker) check() {
	// 检查是否被copy了，如果被copy，那么panic
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
type noCopy struct{}

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
