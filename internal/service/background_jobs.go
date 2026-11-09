package service

import (
	"context"
	"sync"
)

// A single process-wide budget includes real and demo background work. Jobs
// cannot outlive database pools or multiply without an admission bound.
type backgroundJobGroup struct {
	mu sync.Mutex
	closing bool
	ctx context.Context
	cancel context.CancelFunc
	wg sync.WaitGroup
	slots chan struct{}
}
func newBackgroundJobGroup(limit int) *backgroundJobGroup {
	ctx,cancel := context.WithCancel(context.Background())
	return &backgroundJobGroup{ctx:ctx,cancel:cancel,slots:make(chan struct{},limit)}
}
func (g *backgroundJobGroup) start(run func(context.Context)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing { return false }
	select { case g.slots <- struct{}{}: default: return false }
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		run(g.ctx)
	}()
	return true
}
func (g *backgroundJobGroup) stop() {
	g.mu.Lock()
	g.closing = true
	g.cancel()
	g.mu.Unlock()
	g.wg.Wait()
}
var backgroundJobs = newBackgroundJobGroup(8)

// ShutdownBackgroundJobs runs after HTTP requests drain, before pools close.
func ShutdownBackgroundJobs() { backgroundJobs.stop() }
