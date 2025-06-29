package stopwatch

import (
	"time"
)

type Stopwatch struct {
	startTime time.Time
}

func (sw *Stopwatch) Start() {
	sw.startTime = time.Now()
}

func (sw *Stopwatch) Reset() {
	sw.startTime = time.Time{}
}

func (sw *Stopwatch) Elapsed() time.Duration {
	return time.Since(sw.startTime)
}
