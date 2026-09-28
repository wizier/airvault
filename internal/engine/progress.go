package engine

import "sync"

// transferProgress merges partial updates into the Progress the service
// sees: a negative percent keeps the last one, and bytes never go back.
type transferProgress struct {
	mu   sync.Mutex
	last Progress
	sink *latestDispatcher[Progress]
}

func newTransferProgress(fn func(Progress)) *transferProgress {
	return &transferProgress{sink: newLatestDispatcher("backup progress", fn)}
}

func (p *transferProgress) submit(phase ProgressPhase, percent float64, bytes uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last.Phase = phase
	if percent >= 0 {
		p.last.Percent = int(percent)
	}
	p.last.BytesDone = max(p.last.BytesDone, int64(bytes))
	p.sink.submit(p.last)
}

// close delivers the last update before returning.
func (p *transferProgress) close() { p.sink.close() }
