package opennebula

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAdaptiveTimeoutTrackerKeepsStaticFloorBeforeMinSamples(t *testing.T) {
	tracker := NewAdaptiveTimeoutTracker(AdaptiveTimeoutConfig{
		Enabled:              true,
		MinSamples:           4,
		SampleWindow:         8,
		P95MultiplierPercent: 400,
		MaxTimeout:           time.Minute,
	})
	key := NormalizeObservationKey("attach", "local", 10*1024*1024*1024)
	for i := 0; i < 3; i++ {
		tracker.Observe(key, 5*time.Second)
	}

	recommendation := tracker.Recommend(key, 20*time.Second)
	assert.Equal(t, 20*time.Second, recommendation.Recommended)
	assert.Equal(t, 3, recommendation.SampleCount)
}

func TestAdaptiveTimeoutTrackerUsesP95AndMultiplier(t *testing.T) {
	tracker := NewAdaptiveTimeoutTracker(AdaptiveTimeoutConfig{
		Enabled:              true,
		MinSamples:           4,
		SampleWindow:         8,
		P95MultiplierPercent: 400,
		MaxTimeout:           2 * time.Minute,
	})
	key := NormalizeObservationKey("attach", "local", 10*1024*1024*1024)
	for i := 0; i < 4; i++ {
		tracker.Observe(key, 10*time.Second)
	}

	recommendation := tracker.Recommend(key, 20*time.Second)
	assert.Equal(t, 40*time.Second, recommendation.Recommended)
	assert.Equal(t, 4, recommendation.SampleCount)
}

func TestAdaptiveTimeoutTrackerHonorsMaxTimeout(t *testing.T) {
	tracker := NewAdaptiveTimeoutTracker(AdaptiveTimeoutConfig{
		Enabled:              true,
		MinSamples:           2,
		SampleWindow:         8,
		P95MultiplierPercent: 400,
		MaxTimeout:           45 * time.Second,
	})
	key := NormalizeObservationKey("detach", "local", 10*1024*1024*1024)
	tracker.Observe(key, 20*time.Second)
	tracker.Observe(key, 20*time.Second)

	recommendation := tracker.Recommend(key, 15*time.Second)
	assert.Equal(t, 45*time.Second, recommendation.Recommended)
}
