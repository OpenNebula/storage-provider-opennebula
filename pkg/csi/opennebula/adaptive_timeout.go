package opennebula

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

type AdaptiveTimeoutConfig struct {
	Enabled              bool
	MinSamples           int
	SampleWindow         int
	P95MultiplierPercent int
	MaxTimeout           time.Duration
}

type adaptiveObservation struct {
	Duration   time.Duration `json:"duration"`
	ObservedAt time.Time     `json:"observedAt"`
}

type AdaptiveTimeoutTracker struct {
	mu     sync.RWMutex
	config AdaptiveTimeoutConfig
	window map[HotplugObservationKey][]adaptiveObservation
}

func NewAdaptiveTimeoutTracker(cfg AdaptiveTimeoutConfig) *AdaptiveTimeoutTracker {
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = 8
	}
	if cfg.SampleWindow <= 0 {
		cfg.SampleWindow = 20
	}
	if cfg.P95MultiplierPercent <= 0 {
		cfg.P95MultiplierPercent = 400
	}
	return &AdaptiveTimeoutTracker{
		config: cfg,
		window: map[HotplugObservationKey][]adaptiveObservation{},
	}
}

func NormalizeObservationKey(operation, backend string, sizeBytes int64) HotplugObservationKey {
	return HotplugObservationKey{
		Operation:  operation,
		Backend:    backend,
		SizeBucket: SizeBucketForBytes(sizeBytes),
	}
}

func SizeBucketForBytes(sizeBytes int64) string {
	if sizeBytes <= 20*1024*1024*1024 {
		return "le20Gi"
	}
	if sizeBytes <= 100*1024*1024*1024 {
		return "gt20Gi-le100Gi"
	}
	if sizeBytes <= 500*1024*1024*1024 {
		return "gt100Gi-le500Gi"
	}
	return "gt500Gi"
}

func (t *AdaptiveTimeoutTracker) Observe(key HotplugObservationKey, duration time.Duration) {
	if t == nil || !t.config.Enabled || duration <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	window := append(t.window[key], adaptiveObservation{
		Duration:   duration,
		ObservedAt: time.Now().UTC(),
	})
	if len(window) > t.config.SampleWindow {
		window = window[len(window)-t.config.SampleWindow:]
	}
	t.window[key] = window
}

func (t *AdaptiveTimeoutTracker) Recommend(key HotplugObservationKey, staticFloor time.Duration) HotplugTimeoutRecommendation {
	recommendation := HotplugTimeoutRecommendation{
		Key:         key,
		StaticFloor: staticFloor,
		AdaptiveMax: t.maxTimeout(),
		Recommended: staticFloor,
	}
	if t == nil || !t.config.Enabled {
		return recommendation
	}
	t.mu.RLock()
	window := append([]adaptiveObservation(nil), t.window[key]...)
	t.mu.RUnlock()
	recommendation.SampleCount = len(window)
	if len(window) == 0 {
		return recommendation
	}
	recommendation.LastObservedAt = window[len(window)-1].ObservedAt
	if len(window) < t.config.MinSamples {
		return recommendation
	}
	durations := make([]time.Duration, 0, len(window))
	for _, sample := range window {
		durations = append(durations, sample.Duration)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95Index := int(math.Ceil(float64(len(durations))*0.95)) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	if p95Index >= len(durations) {
		p95Index = len(durations) - 1
	}
	recommended := time.Duration((int64(durations[p95Index])*int64(t.config.P95MultiplierPercent) + 99) / 100)
	if recommended < staticFloor {
		recommended = staticFloor
	}
	if maxTimeout := t.maxTimeout(); maxTimeout > 0 && recommended > maxTimeout {
		recommended = maxTimeout
	}
	recommendation.Recommended = recommended
	return recommendation
}

func (t *AdaptiveTimeoutTracker) Snapshot() map[string]HotplugTimeoutRecommendation {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	snapshot := make(map[string]HotplugTimeoutRecommendation, len(t.window))
	for key, samples := range t.window {
		lastObserved := time.Time{}
		if len(samples) > 0 {
			lastObserved = samples[len(samples)-1].ObservedAt
		}
		snapshot[key.String()] = HotplugTimeoutRecommendation{
			Key:            key,
			SampleCount:    len(samples),
			LastObservedAt: lastObserved,
			AdaptiveMax:    t.maxTimeout(),
		}
	}
	return snapshot
}

func (t *AdaptiveTimeoutTracker) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("{}"), nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	payload := make(map[string][]adaptiveObservation, len(t.window))
	for key, samples := range t.window {
		payload[key.String()] = append([]adaptiveObservation(nil), samples...)
	}
	return json.Marshal(payload)
}

func (t *AdaptiveTimeoutTracker) UnmarshalJSON(data []byte) error {
	if t == nil {
		return fmt.Errorf("adaptive timeout tracker is nil")
	}
	payload := map[string][]adaptiveObservation{}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	window := map[HotplugObservationKey][]adaptiveObservation{}
	for key, samples := range payload {
		parsed, err := ParseHotplugObservationKey(key)
		if err != nil {
			return err
		}
		window[parsed] = append([]adaptiveObservation(nil), samples...)
	}
	t.mu.Lock()
	t.window = window
	t.mu.Unlock()
	return nil
}

func (t *AdaptiveTimeoutTracker) maxTimeout() time.Duration {
	if t == nil {
		return 0
	}
	return t.config.MaxTimeout
}

func (k HotplugObservationKey) String() string {
	return fmt.Sprintf("%s|%s|%s", k.Operation, k.Backend, k.SizeBucket)
}

func ParseHotplugObservationKey(raw string) (HotplugObservationKey, error) {
	var key HotplugObservationKey
	parts := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '|' {
			parts = append(parts, raw[start:i])
			start = i + 1
		}
	}
	parts = append(parts, raw[start:])
	if len(parts) != 3 {
		return key, fmt.Errorf("invalid hotplug observation key %q", raw)
	}
	key.Operation = parts[0]
	key.Backend = parts[1]
	key.SizeBucket = parts[2]
	return key, nil
}
