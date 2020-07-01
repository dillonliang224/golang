// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once is an object that will perform exactly one action.
// 开箱即用和并发安全的
type Once struct {
	// done indicates whether the action has been performed.
	// It is first in the struct because it is used in the hot path.
	// The hot path is inlined at every call site.
	// Placing done first allows more compact instructions on some architectures (amd64/x86),
	// and fewer instructions (to calculate offset) on other architectures.
	// 标记方法是否执行过，值不是0就是1
	// 为什么设置为uint32类型呢？因为设置done的值是原子操作
	done uint32
	// 锁，为了并发安全
	m    Mutex
}

// Do calls the function f if and only if Do is being called for the
// first time for this instance of Once. In other words, given
// 	var once Once
// if once.Do(f) is called multiple times, only the first call will invoke f,
// even if f has a different value in each invocation. A new instance of
// Once is required for each function to execute.
//
// Do is intended for initialization that must be run exactly once. Since f
// is niladic, it may be necessary to use a function literal to capture the
// arguments to a function to be invoked by Do:
// 	config.once.Do(func() { config.init(filename) })
//
// Because no call to Do returns until the one call to f returns, if f causes
// Do to be called, it will deadlock.
//
// If f panics, Do considers it to have returned; future calls of Do return
// without calling f.
//
func (o *Once) Do(f func()) {
	// Note: Here is an incorrect implementation of Do:
	//
	//	if atomic.CompareAndSwapUint32(&o.done, 0, 1) {
	//		f()
	//	}
	//
	// Do guarantees that when it returns, f has finished.
	// This implementation would not implement that guarantee:
	// given two simultaneous calls, the winner of the cas would
	// call f, and the second would return immediately, without
	// waiting for the first's call to f to complete.
	// This is why the slow path falls back to a mutex, and why
	// the atomic.StoreUint32 must be delayed until after f returns.
	// 上面的实现方式不能保证当调用Do结束时，f()执行完成

	// 快速失败路径
	// 原子操作获取done的值，如果为1，说明执行过了，什么都不做返回
	if atomic.LoadUint32(&o.done) == 0 {
		// Outlined slow-path to allow inlining of the fast-path.
		o.doSlow(f)
	}
}

func (o *Once) doSlow(f func()) {
	// 并发操作时，为了保证并发安全，先加锁获取资源所有权
	o.m.Lock()
	// 使用defer，保证f()执行完成或者panic后，正常释放锁资源
	defer o.m.Unlock()
	// 二次检查f()释放执行过，0代表未执行，1代表已执行
	if o.done == 0 {
		defer atomic.StoreUint32(&o.done, 1)
		// 先执行函数，后设置done的值（defer）
		// 1。 即使f()执行panic了，done的值也会变为1，如果要设定重试机制，那么需要考虑Once值的适时替换问题
		// 2。 如果f()执行长时间阻塞或者根本就不会结束，那么其他相关goroutine就会阻塞在互斥锁那行代码上。
		f()
	}
}
