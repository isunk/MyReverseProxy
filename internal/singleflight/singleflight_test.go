package singleflight

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGroup_MergesConcurrentCalls(t *testing.T) {
	var group Group[int]
	call, leader := group.Begin("key")
	if !leader {
		t.Fatal("first caller must be leader")
	}
	const followers = 7
	registered := make(chan struct{}, followers)
	var wg sync.WaitGroup
	for i := 0; i < followers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			follower, isLeader := group.Begin("key")
			if isLeader {
				t.Error("caller during in-flight call must be follower")
				group.Finish("key", follower, 0, nil)
				return
			}
			registered <- struct{}{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			value, err := follower.WaitContext(ctx)
			if err != nil || value != 42 {
				t.Errorf("follower got %d, %v", value, err)
			}
		}()
	}
	for i := 0; i < followers; i++ {
		<-registered
	}
	group.Finish("key", call, 42, nil)
	wg.Wait()
}

func TestGroup_FollowerSharesLeaderResult(t *testing.T) {
	var group Group[string]
	errBoom := errors.New("boom")
	leaderReady := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		call, leader := group.Begin("k")
		if !leader {
			t.Error("first caller must be leader")
		}
		close(leaderReady)
		<-leaderDone
		group.Finish("k", call, "value", errBoom)
	}()
	<-leaderReady
	call, leader := group.Begin("k")
	if leader {
		t.Fatal("second caller must be follower")
	}
	close(leaderDone)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	value, err := call.WaitContext(ctx)
	if !errors.Is(err, errBoom) || value != "value" {
		t.Fatalf("Wait = %q, %v, want %q, %v", value, err, "value", errBoom)
	}
}

func TestCall_WaitContextReturnsOnCancel(t *testing.T) {
	var group Group[int]
	call, _ := group.Begin("k")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := call.WaitContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitContext err = %v, want DeadlineExceeded", err)
	}
	group.Finish("k", call, 1, nil)
}

func TestGroup_ReuseAfterFinish(t *testing.T) {
	var group Group[int]
	call, leader := group.Begin("k")
	if !leader {
		t.Fatal("first call must be leader")
	}
	group.Finish("k", call, 1, nil)
	call, leader = group.Begin("k")
	if !leader {
		t.Fatal("call after finish must start fresh as leader")
	}
	group.Finish("k", call, 2, nil)
}
