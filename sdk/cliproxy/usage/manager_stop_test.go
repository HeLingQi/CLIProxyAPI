package usage

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type blockingStopPlugin struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (p *blockingStopPlugin) HandleUsage(context.Context, Record) {
	p.calls.Add(1)
	close(p.entered)
	<-p.release
}

func TestManagerStopWaitsForInFlightDelivery(t *testing.T) {
	manager := NewManager(1)
	plugin := &blockingStopPlugin{entered: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{})

	select {
	case <-plugin.entered:
	case <-time.After(time.Second):
		t.Fatal("usage plugin was not invoked")
	}

	stopped := make(chan struct{})
	go func() {
		manager.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop() returned before the in-flight usage plugin completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(plugin.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop() did not return after the in-flight usage plugin completed")
	}
	if got := plugin.calls.Load(); got != 1 {
		t.Fatalf("plugin calls = %d, want 1", got)
	}
}
