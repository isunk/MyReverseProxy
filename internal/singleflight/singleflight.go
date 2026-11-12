// Package singleflight 合并同 key 的并发调用：首个调用者执行，其余等待并共享同一结果。
package singleflight

import (
	"context"
	"sync"
)

// Group 管理进行中的调用，按 key 合并并发执行，零值可用。
type Group[V any] struct {
	mu    sync.Mutex
	calls map[string]*Call[V]
}

// Call 一次进行中的调用，非首个调用者通过 Wait 共享结果。
type Call[V any] struct {
	done  chan struct{}
	value V
	err   error
}

// Begin 登记一次调用，第二个返回值表示调用方是否为负责执行的第一个等待者。
func (g *Group[V]) Begin(key string) (*Call[V], bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if call, ok := g.calls[key]; ok {
		return call, false
	}
	if g.calls == nil {
		g.calls = map[string]*Call[V]{}
	}
	call := &Call[V]{done: make(chan struct{})}
	g.calls[key] = call
	return call, true
}

// Wait 阻塞至结果就绪并返回。
func (c *Call[V]) Wait() (V, error) {
	<-c.done
	return c.value, c.err
}

// WaitContext 阻塞至结果就绪或 ctx 结束；ctx 结束只放行当前等待者，调用本身继续完成。
func (c *Call[V]) WaitContext(ctx context.Context) (V, error) {
	select {
	case <-c.done:
		return c.value, c.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

// Finish 注销进行中的调用并广播结果，唤醒所有等待者；先赋值后关闭，等待者醒来时结果必已就绪。
func (g *Group[V]) Finish(key string, call *Call[V], value V, err error) {
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	call.value, call.err = value, err
	close(call.done)
}
