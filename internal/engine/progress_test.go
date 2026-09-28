package engine

import (
	"slices"
	"testing"
)

func TestLatestDispatcherCoalescesAndFlushes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var received []int
	dispatcher := newLatestDispatcher("test", func(value int) {
		if value == 1 {
			close(started)
			<-release
		}
		received = append(received, value)
	})
	dispatcher.submit(1)
	<-started
	dispatcher.submit(2)
	dispatcher.submit(3)
	close(release)
	dispatcher.close()

	if !slices.Equal(received, []int{1, 3}) {
		t.Fatalf("received = %v, want [1 3]", received)
	}
}
