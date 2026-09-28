package backup2

type batchPosition struct {
	files       bool
	done, total uint64
}

func (p batchPosition) fraction() float64 {
	if p.total == 0 {
		return 0
	}
	return min(float64(p.done)/float64(p.total), 1)
}

// tracker turns the device's per-batch targets into one monotonic percent,
// interpolating within a batch by its own progress.
type tracker struct {
	anchor       float64
	sessionBytes uint64
	lastEmit     uint64
	lastPercent  float64
}

func (t *tracker) snapshot(batchBytes uint64, position batchPosition, target float64) (percent float64, bytes uint64) {
	overall := t.anchor
	if target > t.anchor {
		overall += (target - t.anchor) * position.fraction()
	}
	t.lastPercent = max(overall, t.lastPercent)
	if t.lastPercent <= 0 {
		return -1, t.sessionBytes + batchBytes
	}
	return t.lastPercent, t.sessionBytes + batchBytes
}

func (t *tracker) finishBatch(batchBytes uint64, target float64) {
	t.sessionBytes += batchBytes
	t.lastEmit = t.sessionBytes
	t.anchor = max(t.anchor, target)
}

func (t *tracker) shouldEmit(batchBytes uint64) bool {
	done := t.sessionBytes + batchBytes
	if done-t.lastEmit < emitEvery {
		return false
	}
	t.lastEmit = done
	return true
}
