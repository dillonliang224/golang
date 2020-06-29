// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// 参考资料：
// https://blog.csdn.net/u010853261/article/details/106293258
// https://colobu.com/2018/12/18/dive-into-sync-mutex/
// https://studygolang.com/articles/26030

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
// 一旦使用，不能copy
type Mutex struct {
	// 当前互斥锁的状态，0表示未获取锁，1表示得到锁
	state int32
	// 用于控制锁状态的信号量
	sema uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 1， 0001，最后一位表示当前锁的状态，0未锁，1已锁
	mutexLocked = 1 << iota // mutex is locked
	// 2， 0010，倒数第二位表示当前锁是否被唤醒，0唤醒，1未唤醒
	mutexWoken
	// 4， 0100， 倒数第三位表示当前对象是否为饥饿模式，0正常，1饥饿
	mutexStarving

	// 从倒数第四位往前的bit表示排队的gorouting的数量
	mutexWaiterShift = iota

	// Mutex fairness.
	// 互斥锁
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// 锁有两种模式： 正常模式、饥饿模式
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	// 正常模式下，所有的等待者按照FIFO的顺序排列，被唤醒的等待者并不一定获取锁，它需要和正在CPU上
	// 运行的goroutine竞争锁。并且很可能竞争失败，因为已在CPU上的goroutine拥有更多优势（已在CPU上），
	// 如果这个排在队列头部的goroutine竞争失败且超过1ms，那么这个锁会切换为饥饿模式。
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// 饥饿模式下，对列头部的goroutine在下次获取锁时直接得到，不存在竞争关系。
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	// 新来的goroutine将不会尝试获取锁，即使锁看起来是unlock状态，也不会尝试自旋，而是直接放在
	// 等待队列的尾部。
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	// 如果等待队列里的goroutine获取了锁，并且是以下任一情况：
	// 1. 这个goroutine是等待队列最后一个
	// 2. 这个goroutine等待时间小于1ms
	// 那么，这个锁会切换为正常模式。
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// 正常模式有很好的性能
	// Starvation mode is important to prevent pathological cases of tail latency.
	// 饥饿模式可以有效的阻止尾部延迟的现象，防止协程等待锁的时间过长。
	// 饥饿的阀值
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 如果mutex的state没有被锁，也没有等待/唤醒的goroutine, 锁处于正常状态，那么获得锁，返回.
	// 比如锁第一次被goroutine请求时，就是这种状态。或者锁处于空闲的时候，也是这种状态。
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	// 开始等待时间
	var waitStartTime int64
	// 是否饥饿模式
	starving := false
	// 是否已唤醒
	awoke := false
	// 自旋次数
	iter := 0
	// 锁的当前状态
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 1. mutex已经被锁了并且不是饥饿模式
		// mutexLocked|mutexStarving = 0101
		// 如果是饥饿模式，那么old = 01**，那么做与操作，肯定不等于mutexLocked，所以此处排除饥饿模式
		// 2. 可以自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 已经获得锁了并且不是饥饿模式 && 可以自旋，与cpu核数有关
			// 自旋的过程中，如果发现state还没有设置woken标识，则设置它的woken标识，并标记自己为被唤醒
			// old&mutexWoken判断是否设置woken标识，0代表未设置
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 触发自旋
			runtime_doSpin()
			// 自旋次数加1
			iter++
			old = m.state
			continue
		}

		// 到了这一步， state的状态可能是：
		// 1. 锁还没有被释放，锁处于正常状态
		// 2. 锁还没有被释放， 锁处于饥饿状态
		// 3. 锁已经被释放， 锁处于正常状态
		// 4. 锁已经被释放， 锁处于饥饿状态

		// new 复制state的当前状态，用来设置新的状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 原协程已释放锁，且old是非饥饿状态
		// old state状态是非饥饿状态，new state设置锁标识
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}

		// old处于锁状态或者饥饿状态，那么新的goroutine排队
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 如果当前goroutine处于饥饿模式，且有锁标识，即0**1，设置new state为饥饿模式
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}

		// 如果当前goroutine已经设置为唤醒状态, 需要清除new state的唤醒标记, 因为本goroutine要么获得了锁，要么进入休眠，
		// 总之state的新状态不再是woken状态.
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}

		// 通过CAS设置new state值。
		// 注意new的锁标记不一定是true, 也可能只是标记一下锁的state是饥饿状态.
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				// 如果old state的状态是未被锁状态，并且锁不处于饥饿状态,
				// 那么当前goroutine已经获取了锁的拥有权，返回
				// 这里代表的是正常模式获取锁成功
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 计算当前goroutine的等待时间
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}

			// 既然未能获取到锁， 那么就使用sleep原语阻塞本goroutine
			// 如果是新来的goroutine,queueLifo=false, 加入到等待队列的尾部，耐心等待
			// 如果是唤醒的goroutine, queueLifo=true, 加入到等待队列的头部
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// sleep之后，此goroutine被唤醒

			// 计算当前goroutine是否已经处于饥饿状态.
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 当前锁状态
			old = m.state

			// 如果当前锁是饥饿模式
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}

				// 减去当前锁的排队数
				delta := int32(mutexLocked - 1<<mutexWaiterShift)

				// 如果本goroutine是最后一个等待者，或者它并不处于饥饿状态，
				// 那么我们需要把锁的state状态设置为正常模式.
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 退出饥饿模式
					delta -= mutexStarving
				}

				// 修改状态，终止
				atomic.AddInt32(&m.state, delta)
				break
			}

			// 设置新的state，因为已经得到了锁，退出，返回
			awoke = true
			iter = 0
		} else {
			// 如果CAS设置不成功，重新获取锁的state，从for循环开始重新开始
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// new == 0 ，当前goroutine成功了解锁了互斥锁
	// new != 0，慢速解锁
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 如果已经被解锁了，panic
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}

	// 是否为饥饿模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 如果互斥锁不存在等待者或者互斥锁的 mutexLocked、mutexStarving、mutexWoken 状态不都为 0，
			// 那么当前方法就可以直接返回，不需要唤醒其他等待者；
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}

			// Grab the right to wake someone.
			// 如果互斥锁存在等待者，会通过 sync.runtime_Semrelease 唤醒等待者并移交锁的所有权；
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 饥饿模型下， 会把锁的所有权交给下一个尝试获取锁的等待者
		// 等待者被唤醒后会获取锁并设置锁标识，这是，锁还没有退出饥饿状态，新来的goroutine不会获取锁
		runtime_Semrelease(&m.sema, true, 1)
	}
}
