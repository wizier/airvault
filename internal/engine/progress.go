package engine

import "sync"

// backupCallback is owned by one native call. The small mutex merges partial
// native updates; application code runs later on the dispatcher's Go goroutine.
type backupCallback struct {
	mu   sync.Mutex
	last Progress
	sink *latestDispatcher[Progress]
}

func newBackupCallback(fn func(Progress)) *backupCallback {
	return &backupCallback{sink: newLatestDispatcher("backup progress", fn)}
}

// Native frames carry only what they moved, so the run total is accumulated
// here — ahead of the dispatcher, whose coalescing would otherwise drop bytes.
func (c *backupCallback) submit(phase int32, percent float64, bytes uint64) {
	c.mu.Lock()
	p := c.last
	if phase == 1 {
		p.Phase = ProgressPhaseSealing
	} else {
		p.Phase = ProgressPhaseTransfer
	}
	if percent >= 0 {
		p.Percent = int(percent)
	}
	p.BytesDone += int64(bytes)
	c.last = p
	c.sink.submit(p)
	c.mu.Unlock()
}
