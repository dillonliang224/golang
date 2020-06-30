// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

import (
	"unsafe"
)

// A Value provides an atomic load and store of a consistently typed value.
// The zero value for a Value returns nil from Load.
// Once Store has been called, a Value must not be copied.
//
// A Value must not be copied after first use.
type Value struct {
	v interface{}
}

// ifaceWords is interface{} internal representation.
type ifaceWords struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// Load returns the value set by the most recent Store.
// It returns nil if there has been no call to Store for this Value.
func (v *Value) Load() (x interface{}) {
	vp := (*ifaceWords)(unsafe.Pointer(v))
	typ := LoadPointer(&vp.typ)
	// 如果typ为nil或者中间类型^uintptr(0)，那么说明第一次写入还没有完成，那就直接返回nil
	if typ == nil || uintptr(typ) == ^uintptr(0) {
		// First store not yet completed.
		return nil
	}

	// 到这里，说明第一次写入已成功
	// 根据已有到typ和data，构建一个interface返回
	data := LoadPointer(&vp.data)
	xp := (*ifaceWords)(unsafe.Pointer(&x))
	xp.typ = typ
	xp.data = data
	return
}

// Store sets the value of the Value to x.
// All calls to Store for a given Value must use values of the same concrete type.
// Store of an inconsistent type panics, as does Store(nil).
func (v *Value) Store(x interface{}) {
	// 存储的值不能是nil
	if x == nil {
		panic("sync/atomic: store of nil value into Value")
	}

	// old value
	vp := (*ifaceWords)(unsafe.Pointer(v))
	// new value
	xp := (*ifaceWords)(unsafe.Pointer(&x))
	for {
		// 通过原子操作获取当前Value中存储的类型
		typ := LoadPointer(&vp.typ)

		// 第一次写入
		if typ == nil {
			// Attempt to start first store.
			// Disable preemption so that other goroutines can use
			// active spin wait to wait for completion; and so that
			// GC does not see the fake type accidentally.
			// 如果typ是nil，那么这是第一次store
			// 禁止运行时抢占，其他goroutine可以进行自旋，直到第一次写入成功
			// runtime_procPin()，它可以将一个goroutine死死占用当前使用的P(P-M-G中的processor)，不允许其它goroutine/M抢占,
			// 使得它在执行当前逻辑的时候不被打断，以便可以尽快地完成工作，因为别人一直在等待它。
			// 另一方面，在禁止抢占期间，GC 线程也无法被启用，这样可以防止 GC 线程看到一个莫名其妙的指向^uintptr(0)的类型（这是赋值过程中的中间状态）。
			runtime_procPin()
			// 使用CAS操作，原子性设置typ为^uintptr(0)这个中间状态。
			// 如果失败，则证明已经有别的线程抢先完成了赋值操作，那它就解除抢占锁，然后重新回到 for 循环第一步。
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(^uintptr(0))) {
				// 解除禁止运行时抢占
				runtime_procUnpin()
				continue
			}
			// Complete first store.
			// 如果CAS设置成功，证明当前goroutine获取到了这个"乐观锁"，可以安全地把v设为传入的新值
			StorePointer(&vp.data, xp.data)
			StorePointer(&vp.typ, xp.typ)
			// 解除禁止运行时抢占
			runtime_procUnpin()
			return
		}

		// 写入进行中...
		// 如果看到typ字段还是^uintptr(0)这个中间类型，证明刚刚的第一次写入还没有完成，
		// 所以它会继续循环，“忙等"到第一次写入完成。
		if uintptr(typ) == ^uintptr(0) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}

		// 走到这里的时候，说明第一次写入已完成
		// First store completed. Check type and overwrite data.
		// 首先检查上一次写入的类型与这一次要写入的类型是否一致，如果不一致则抛出异常。
		if typ != xp.typ {
			panic("sync/atomic: store of inconsistently typed value into Value")
		}

		// 直接把这一次要写入的值写入到data字段
		StorePointer(&vp.data, xp.data)
		return
	}
}

// Disable/enable preemption, implemented in runtime.
func runtime_procPin()
func runtime_procUnpin()
