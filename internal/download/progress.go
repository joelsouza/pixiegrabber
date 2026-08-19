package download

import (
	"fmt"
	"sync"
	"time"
)

// progressInterval is the time between two download counts. One event for
// each file would make a very long log for a large Collection, so the count
// is periodic and only a failure is reported at once.
const progressInterval = 2 * time.Second

// downloadProgress counts finished References. The download workers call it
// from more than one goroutine.
type downloadProgress struct {
	mu       sync.Mutex
	reporter Reporter
	clock    func() time.Time
	total    int
	finished int
	failed   int
	lastAt   time.Time
	reported int
}

func newDownloadProgress(reporter Reporter, total int, clock func() time.Time) *downloadProgress {
	if clock == nil {
		clock = time.Now
	}
	return &downloadProgress{reporter: reporter, clock: clock, total: total, lastAt: clock()}
}

// done counts one finished Reference and reports a failure at once.
func (p *downloadProgress) done(result Result) {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	p.finished++
	failure := firstFailure(result)
	if failure != nil {
		p.failed++
	}
	doneNow, failedNow, total := p.finished, p.failed, p.total
	report := p.clock().Sub(p.lastAt) >= progressInterval
	if report {
		p.lastAt = p.clock()
		p.reported = doneNow
	}
	p.mu.Unlock()

	// A failure is reported at once, because the person must not wait for it.
	if failure != nil {
		p.reporter.Progress(
			fmt.Sprintf("Download failed: reference %s (%s).", result.ReferenceID, failure.Code),
			"download_failed",
			map[string]any{"reference": result.ReferenceID, "code": failure.Code},
		)
	}
	if report {
		p.reporter.Progress(
			fmt.Sprintf("Downloading: %d/%d done, %d failed.", doneNow, total, failedNow),
			"download",
			map[string]any{"done": doneNow, "of": total, "failed": failedNow},
		)
	}
}

// finish reports the last count when it differs from the last report.
func (p *downloadProgress) finish() {
	if p == nil || p.reporter == nil {
		return
	}
	p.mu.Lock()
	doneNow, failedNow, total, reported := p.finished, p.failed, p.total, p.reported
	p.mu.Unlock()
	if doneNow == reported {
		return
	}
	p.reporter.Progress(
		fmt.Sprintf("Downloading: %d/%d done, %d failed.", doneNow, total, failedNow),
		"download",
		map[string]any{"done": doneNow, "of": total, "failed": failedNow},
	)
}

// firstFailure returns the failure of one Reference, or the first failure of
// one of its Placements.
func firstFailure(result Result) *Failure {
	if result.Failure != nil {
		return result.Failure
	}
	for _, placement := range result.Placements {
		if placement.Failure != nil {
			return placement.Failure
		}
	}
	return nil
}
