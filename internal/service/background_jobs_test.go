package service

import (
	"context"
	"testing"
	"time"
)
func TestBackgroundJobsBoundCapacityAndJoinOnShutdown(t *testing.T) {
	g:=newBackgroundJobGroup(1)
	started:=make(chan struct{})
	finished:=make(chan struct{})
	if !g.start(func(ctx context.Context) { close(started); <-ctx.Done(); close(finished) }) { t.Fatal("first job rejected") }
	<-started
	if g.start(func(context.Context){}) { t.Fatal("unbounded job admitted") }
	done:=make(chan struct{})
	go func(){g.stop();close(done)}()
	select { case <-done: case <-time.After(time.Second):t.Fatal("shutdown did not cancel and join") }
	select {case <-finished:default:t.Fatal("shutdown returned before job exited")}
	if g.start(func(context.Context){}) {t.Fatal("job admitted after shutdown")}
}
