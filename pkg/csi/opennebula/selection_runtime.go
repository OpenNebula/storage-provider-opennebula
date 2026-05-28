package opennebula

import (
	"sort"
	"sync"
	"time"
)

const datastoreFailureWindow = 15 * time.Minute

type SelectionScore struct {
	DatastoreID        int
	TotalScore         float64
	FreeRatioScore     float64
	AbsoluteFreeScore  float64
	FailurePenalty     float64
	InFlightPenalty    float64
	RecentFailureCount int
	InFlightCount      int
}

type datastoreSelectionRuntime struct {
	mu        sync.Mutex
	failures  map[int][]time.Time
	inFlight  map[int]int
	lastScore map[int]SelectionScore
}

var globalDatastoreSelectionRuntime = &datastoreSelectionRuntime{
	failures:  map[int][]time.Time{},
	inFlight:  map[int]int{},
	lastScore: map[int]SelectionScore{},
}

func (r *datastoreSelectionRuntime) beginAttempt(datastoreID int) func() {
	r.mu.Lock()
	r.inFlight[datastoreID]++
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.inFlight[datastoreID] <= 1 {
			delete(r.inFlight, datastoreID)
			return
		}
		r.inFlight[datastoreID]--
	}
}

func (r *datastoreSelectionRuntime) recordResult(datastoreID int, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.pruneLocked(now)
	if success {
		return
	}

	r.failures[datastoreID] = append(r.failures[datastoreID], now)
}

func (r *datastoreSelectionRuntime) snapshot(now time.Time) (map[int]int, map[int]int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.pruneLocked(now)

	failures := make(map[int]int, len(r.failures))
	for datastoreID, timestamps := range r.failures {
		failures[datastoreID] = len(timestamps)
	}

	inFlight := make(map[int]int, len(r.inFlight))
	for datastoreID, count := range r.inFlight {
		inFlight[datastoreID] = count
	}

	return failures, inFlight
}

func (r *datastoreSelectionRuntime) rememberScores(scores []SelectionScore) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lastScore = make(map[int]SelectionScore, len(scores))
	for _, score := range scores {
		r.lastScore[score.DatastoreID] = score
	}
}

func (r *datastoreSelectionRuntime) pruneLocked(now time.Time) {
	cutoff := now.Add(-datastoreFailureWindow)
	for datastoreID, timestamps := range r.failures {
		filtered := timestamps[:0]
		for _, ts := range timestamps {
			if ts.After(cutoff) {
				filtered = append(filtered, ts)
			}
		}
		if len(filtered) == 0 {
			delete(r.failures, datastoreID)
			continue
		}
		r.failures[datastoreID] = append([]time.Time(nil), filtered...)
	}
}

func beginDatastoreAttempt(datastoreID int) func() {
	return globalDatastoreSelectionRuntime.beginAttempt(datastoreID)
}

func recordDatastoreProvisioningResult(datastoreID int, success bool) {
	globalDatastoreSelectionRuntime.recordResult(datastoreID, success)
}

func resetDatastoreSelectionRuntime() {
	globalDatastoreSelectionRuntime = &datastoreSelectionRuntime{
		failures:  map[int][]time.Time{},
		inFlight:  map[int]int{},
		lastScore: map[int]SelectionScore{},
	}
}

type autopilotDatastoreSelector struct{}

func (autopilotDatastoreSelector) Sort(candidates []Datastore) []Datastore {
	sorted := append([]Datastore(nil), candidates...)
	if len(sorted) < 2 {
		return sorted
	}

	now := time.Now()
	failures, inFlight := globalDatastoreSelectionRuntime.snapshot(now)

	var maxFreeBytes float64
	var maxFailures float64
	var maxInFlight float64
	for _, candidate := range sorted {
		if free := float64(candidate.FreeBytes); free > maxFreeBytes {
			maxFreeBytes = free
		}
		if failures := float64(failures[candidate.ID]); failures > maxFailures {
			maxFailures = failures
		}
		if count := float64(inFlight[candidate.ID]); count > maxInFlight {
			maxInFlight = count
		}
	}

	scores := make([]SelectionScore, 0, len(sorted))
	scoreByID := make(map[int]SelectionScore, len(sorted))
	for _, candidate := range sorted {
		freeRatio := 0.0
		if candidate.TotalBytes > 0 {
			freeRatio = float64(candidate.FreeBytes) / float64(candidate.TotalBytes)
		}
		absoluteFree := 0.0
		if maxFreeBytes > 0 {
			absoluteFree = float64(candidate.FreeBytes) / maxFreeBytes
		}
		failurePenalty := 0.0
		if maxFailures > 0 {
			failurePenalty = float64(failures[candidate.ID]) / maxFailures
		}
		inFlightPenalty := 0.0
		if maxInFlight > 0 {
			inFlightPenalty = float64(inFlight[candidate.ID]) / maxInFlight
		}

		score := SelectionScore{
			DatastoreID:        candidate.ID,
			FreeRatioScore:     freeRatio * 0.4,
			AbsoluteFreeScore:  absoluteFree * 0.3,
			FailurePenalty:     failurePenalty * 0.2,
			InFlightPenalty:    inFlightPenalty * 0.1,
			RecentFailureCount: failures[candidate.ID],
			InFlightCount:      inFlight[candidate.ID],
		}
		score.TotalScore = score.FreeRatioScore + score.AbsoluteFreeScore - score.FailurePenalty - score.InFlightPenalty
		scoreByID[candidate.ID] = score
		scores = append(scores, score)
	}

	globalDatastoreSelectionRuntime.rememberScores(scores)

	sort.SliceStable(sorted, func(i, j int) bool {
		left := scoreByID[sorted[i].ID]
		right := scoreByID[sorted[j].ID]
		if left.TotalScore == right.TotalScore {
			return false
		}
		return left.TotalScore > right.TotalScore
	})

	return sorted
}
