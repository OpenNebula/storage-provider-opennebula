package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

const hotplugQueueStateConfigMapName = "opennebula-csi-hotplug-queue-state"

type HotplugQueueTimeoutError struct {
	Node      string
	Operation string
	Volume    string
	Timeout   time.Duration
}

func (e *HotplugQueueTimeoutError) Error() string {
	if e == nil {
		return "hotplug queue timeout"
	}
	return fmt.Sprintf("timed out after %s waiting for hotplug queue on node %s for %s of volume %s", e.Timeout, e.Node, e.Operation, e.Volume)
}

type HotplugQueueActiveTimeoutError struct {
	Node      string
	Operation string
	Volume    string
	Timeout   time.Duration
}

func (e *HotplugQueueActiveTimeoutError) Error() string {
	if e == nil {
		return "hotplug queue active timeout"
	}
	return fmt.Sprintf("timed out after %s executing hotplug queue request on node %s for %s of volume %s", e.Timeout, e.Node, e.Operation, e.Volume)
}

type HotplugQueueStaleRequestError struct {
	Node      string
	Operation string
	Volume    string
	Reason    string
	Message   string
}

func (e *HotplugQueueStaleRequestError) Error() string {
	if e == nil {
		return "stale hotplug queue request"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Reason != "" {
		return fmt.Sprintf("stale hotplug queue request for %s of volume %s on node %s: %s", e.Operation, e.Volume, e.Node, e.Reason)
	}
	return fmt.Sprintf("stale hotplug queue request for %s of volume %s on node %s", e.Operation, e.Volume, e.Node)
}

type HotplugQueuePausedError struct {
	Node      string
	Operation string
	Volume    string
	Reason    string
	Message   string
}

func (e *HotplugQueuePausedError) Error() string {
	if e == nil {
		return "hotplug queue request paused"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Reason != "" {
		return fmt.Sprintf("hotplug queue request for %s of volume %s on node %s is paused: %s", e.Operation, e.Volume, e.Node, e.Reason)
	}
	return fmt.Sprintf("hotplug queue request for %s of volume %s on node %s is paused", e.Operation, e.Volume, e.Node)
}

type HotplugQueuePriority string

const (
	hotplugQueuePriorityCritical   HotplugQueuePriority = "critical"
	hotplugQueuePriorityNormal     HotplugQueuePriority = "normal"
	hotplugQueuePriorityBackground HotplugQueuePriority = "background"
)

type HotplugQueueValidationDecision string

const (
	HotplugQueueValidationValid     HotplugQueueValidationDecision = "valid"
	HotplugQueueValidationCompleted HotplugQueueValidationDecision = "completed"
	HotplugQueueValidationStale     HotplugQueueValidationDecision = "stale"
	HotplugQueueValidationPaused    HotplugQueueValidationDecision = "paused"
)

type HotplugQueueValidation struct {
	Decision HotplugQueueValidationDecision
	Reason   string
	Message  string
}

type HotplugQueueValidator func(ctx context.Context, node, operation, volume string) HotplugQueueValidation

type HotplugQueueItemSnapshot struct {
	ID         uint64               `json:"id"`
	Node       string               `json:"node"`
	Operation  string               `json:"operation"`
	Volume     string               `json:"volume"`
	Priority   HotplugQueuePriority `json:"priority"`
	EnqueuedAt time.Time            `json:"enqueuedAt"`
	StartedAt  *time.Time           `json:"startedAt,omitempty"`
	Coalesced  int                  `json:"coalesced,omitempty"`
}

type HotplugQueueNodeSnapshot struct {
	Node             string                     `json:"node"`
	Active           *HotplugQueueItemSnapshot  `json:"active,omitempty"`
	Queued           []HotplugQueueItemSnapshot `json:"queued,omitempty"`
	QueuedCount      int                        `json:"queuedCount"`
	OldestAgeSeconds int64                      `json:"oldestAgeSeconds"`
	ActiveAgeSeconds int64                      `json:"activeAgeSeconds,omitempty"`
}

type hotplugQueueRequest struct {
	id         uint64
	node       string
	operation  string
	volume     string
	priority   HotplugQueuePriority
	enqueuedAt time.Time
	startedAt  time.Time
	waitLimit  time.Duration
	coalesced  int
	ctx        context.Context
	fn         func(context.Context) error
	started    chan struct{}
	done       chan struct{}
	once       sync.Once
	err        error
}

type hotplugNodeQueue struct {
	active  *hotplugQueueRequest
	pending []*hotplugQueueRequest
	running bool
}

type HotplugQueueManager struct {
	mu          sync.Mutex
	runtime     *KubeRuntime
	namespace   string
	metrics     *DriverMetrics
	maxWait     time.Duration
	ageBoost    time.Duration
	dedupe      bool
	perItemWait time.Duration
	maxWaitCap  time.Duration
	maxActive   time.Duration
	validator   HotplugQueueValidator
	seq         atomic.Uint64
	nodes       map[string]*hotplugNodeQueue

	snapshotMu       sync.Mutex
	snapshotDebounce time.Duration
	pendingSnapshots map[string]HotplugQueueNodeSnapshot
	snapshotTimers   map[string]*time.Timer
}

func NewHotplugQueueManager(runtime *KubeRuntime, namespace string, metrics *DriverMetrics, maxWait, ageBoost time.Duration) *HotplugQueueManager {
	return &HotplugQueueManager{
		runtime:   runtime,
		namespace: namespace,
		metrics:   metrics,
		maxWait:   maxWait,
		ageBoost:  ageBoost,
		dedupe:    true,
		nodes:     map[string]*hotplugNodeQueue{},
	}
}

func (m *HotplugQueueManager) Configure(dedupe bool, perItemWait, maxWaitCap, maxActive time.Duration, validator HotplugQueueValidator) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dedupe = dedupe
	m.perItemWait = perItemWait
	m.maxWaitCap = maxWaitCap
	m.maxActive = maxActive
	m.validator = validator
}

func (m *HotplugQueueManager) SetSnapshotDebounce(debounce time.Duration) {
	if m == nil {
		return
	}
	if debounce < 0 {
		debounce = 0
	}
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	m.snapshotDebounce = debounce
}

func (m *HotplugQueueManager) Run(ctx context.Context, node, operation, volume string, priority HotplugQueuePriority, fn func(context.Context) error) error {
	if m == nil || node == "" {
		return fn(ctx)
	}
	req := &hotplugQueueRequest{
		id:         m.seq.Add(1),
		node:       node,
		operation:  operation,
		volume:     volume,
		priority:   priority,
		enqueuedAt: time.Now().UTC(),
		ctx:        ctx,
		fn:         fn,
		started:    make(chan struct{}),
		done:       make(chan struct{}),
	}

	req, coalesced := m.enqueue(req)
	waitLimit := req.waitLimit
	waitCtx := ctx
	var cancel context.CancelFunc
	if waitLimit > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, waitLimit)
		defer cancel()
	}

	if coalesced {
		select {
		case <-req.done:
			outcome := "coalesced_success"
			if req.err != nil {
				outcome = "coalesced_error"
			}
			m.metrics.RecordHotplugQueueWait(operation, outcome, time.Since(req.enqueuedAt))
			m.metrics.RecordHotplugQueueDispatch(operation, string(priority), outcome)
			return req.err
		case <-waitCtx.Done():
			m.metrics.RecordHotplugQueueWait(operation, "coalesced_timeout", time.Since(req.enqueuedAt))
			m.metrics.RecordHotplugQueueDispatch(operation, string(priority), "coalesced_timeout")
			return &HotplugQueueTimeoutError{
				Node:      node,
				Operation: operation,
				Volume:    volume,
				Timeout:   waitLimit,
			}
		}
	}

	select {
	case <-req.started:
	case <-req.done:
		return req.err
	case <-waitCtx.Done():
		if m.cancel(req.id, node) {
			m.metrics.RecordHotplugQueueWait(operation, "timeout", time.Since(req.enqueuedAt))
			m.metrics.RecordHotplugQueueDispatch(operation, string(priority), "timeout")
			return &HotplugQueueTimeoutError{
				Node:      node,
				Operation: operation,
				Volume:    volume,
				Timeout:   waitLimit,
			}
		}
	}

	<-req.done
	err := req.err
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.metrics.RecordHotplugQueueWait(operation, outcome, time.Since(req.enqueuedAt))
	m.metrics.RecordHotplugQueueDispatch(operation, string(priority), outcome)
	return err
}

func (m *HotplugQueueManager) Snapshot() map[string]HotplugQueueNodeSnapshot {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]HotplugQueueNodeSnapshot, len(m.nodes))
	for node := range m.nodes {
		out[node] = m.snapshotLocked(node)
	}
	return out
}

func (m *HotplugQueueManager) HasVolume(volume string) bool {
	if m == nil || volume == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, nodeQueue := range m.nodes {
		if nodeQueue == nil {
			continue
		}
		if nodeQueue.active != nil && nodeQueue.active.volume == volume {
			return true
		}
		for _, req := range nodeQueue.pending {
			if req.volume == volume {
				return true
			}
		}
	}
	return false
}

func (m *HotplugQueueManager) enqueue(req *hotplugQueueRequest) (*hotplugQueueRequest, bool) {
	m.mu.Lock()
	nodeQueue := m.nodes[req.node]
	if nodeQueue == nil {
		nodeQueue = &hotplugNodeQueue{}
		m.nodes[req.node] = nodeQueue
	}
	if m.dedupe {
		if existing := findDuplicateRequest(nodeQueue, req); existing != nil {
			existing.coalesced++
			snapshot := m.snapshotLocked(req.node)
			m.mu.Unlock()
			m.metrics.RecordHotplugQueueDispatch(req.operation, string(req.priority), "coalesced")
			m.metrics.RecordHotplugQueueCoalesced(req.operation, "joined")
			m.persistSnapshot(snapshot)
			return existing, true
		}
	}
	req.waitLimit = m.effectiveWaitLimitLocked(nodeQueue)
	nodeQueue.pending = append(nodeQueue.pending, req)
	shouldStart := !nodeQueue.running
	if shouldStart {
		nodeQueue.running = true
	}
	snapshot := m.snapshotLocked(req.node)
	m.recordDepthMetricsLocked()
	m.mu.Unlock()

	m.persistSnapshot(snapshot)
	if shouldStart {
		go m.runNodeQueue(req.node)
	}
	return req, false
}

func (m *HotplugQueueManager) cancel(id uint64, node string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodeQueue := m.nodes[node]
	if nodeQueue == nil {
		return false
	}
	for idx, req := range nodeQueue.pending {
		if req.id != id {
			continue
		}
		nodeQueue.pending = slices.Delete(nodeQueue.pending, idx, idx+1)
		snapshot := m.snapshotLocked(node)
		m.recordDepthMetricsLocked()
		go m.persistSnapshot(snapshot)
		req.complete(&HotplugQueueTimeoutError{
			Node:      node,
			Operation: req.operation,
			Volume:    req.volume,
			Timeout:   req.waitLimit,
		})
		return true
	}
	return false
}

func (m *HotplugQueueManager) runNodeQueue(node string) {
	for {
		req := m.next(node)
		if req == nil {
			return
		}
		close(req.started)
		err := m.runRequest(req)
		req.complete(err)
		m.finish(node, req.id)
	}
}

func (m *HotplugQueueManager) runRequest(req *hotplugQueueRequest) error {
	if m == nil || req == nil {
		return nil
	}
	if m.validator != nil {
		validation := m.validator(req.ctx, req.node, req.operation, req.volume)
		switch validation.Decision {
		case HotplugQueueValidationCompleted:
			m.metrics.RecordHotplugQueueDispatch(req.operation, string(req.priority), "completed")
			return nil
		case HotplugQueueValidationStale:
			m.metrics.RecordHotplugQueueDispatch(req.operation, string(req.priority), "stale")
			return &HotplugQueueStaleRequestError{
				Node:      req.node,
				Operation: req.operation,
				Volume:    req.volume,
				Reason:    validation.Reason,
				Message:   validation.Message,
			}
		case HotplugQueueValidationPaused:
			m.metrics.RecordHotplugQueueDispatch(req.operation, string(req.priority), "paused")
			return &HotplugQueuePausedError{
				Node:      req.node,
				Operation: req.operation,
				Volume:    req.volume,
				Reason:    validation.Reason,
				Message:   validation.Message,
			}
		}
	}

	runCtx := req.ctx
	var cancel context.CancelFunc
	if m.maxActive > 0 {
		runCtx, cancel = context.WithTimeout(req.ctx, m.maxActive)
		defer cancel()
	}
	err := req.fn(runCtx)
	if err != nil {
		if m.maxActive > 0 && errors.Is(err, context.DeadlineExceeded) {
			return &HotplugQueueActiveTimeoutError{
				Node:      req.node,
				Operation: req.operation,
				Volume:    req.volume,
				Timeout:   m.maxActive,
			}
		}
		return err
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return &HotplugQueueActiveTimeoutError{
			Node:      req.node,
			Operation: req.operation,
			Volume:    req.volume,
			Timeout:   m.maxActive,
		}
	}
	return nil
}

func (m *HotplugQueueManager) next(node string) *hotplugQueueRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodeQueue := m.nodes[node]
	if nodeQueue == nil || len(nodeQueue.pending) == 0 {
		if nodeQueue != nil {
			nodeQueue.running = false
			delete(m.nodes, node)
			m.recordDepthMetricsLocked()
			go m.persistSnapshot(HotplugQueueNodeSnapshot{Node: node})
		}
		return nil
	}

	now := time.Now()
	selectedIdx := 0
	selectedScore := m.effectivePriority(nodeQueue.pending[0], now)
	for idx := 1; idx < len(nodeQueue.pending); idx++ {
		score := m.effectivePriority(nodeQueue.pending[idx], now)
		if score < selectedScore {
			selectedIdx = idx
			selectedScore = score
		}
	}
	req := nodeQueue.pending[selectedIdx]
	nodeQueue.pending = slices.Delete(nodeQueue.pending, selectedIdx, selectedIdx+1)
	nodeQueue.active = req
	req.startedAt = time.Now().UTC()
	snapshot := m.snapshotLocked(node)
	m.recordDepthMetricsLocked()
	go m.persistSnapshot(snapshot)
	return req
}

func (m *HotplugQueueManager) finish(node string, id uint64) {
	m.mu.Lock()
	nodeQueue := m.nodes[node]
	if nodeQueue != nil && nodeQueue.active != nil && nodeQueue.active.id == id {
		nodeQueue.active = nil
	}
	snapshot := m.snapshotLocked(node)
	m.recordDepthMetricsLocked()
	m.mu.Unlock()
	m.persistSnapshot(snapshot)
}

func (m *HotplugQueueManager) effectivePriority(req *hotplugQueueRequest, now time.Time) int {
	base := 2
	switch req.priority {
	case hotplugQueuePriorityCritical:
		base = 0
	case hotplugQueuePriorityNormal:
		base = 1
	}
	if m.ageBoost <= 0 {
		return base
	}
	boosts := int(now.Sub(req.enqueuedAt) / m.ageBoost)
	if boosts > base {
		boosts = base
	}
	return base - boosts
}

func (m *HotplugQueueManager) effectiveWaitLimitLocked(nodeQueue *hotplugNodeQueue) time.Duration {
	wait := m.maxWait
	if wait <= 0 {
		return 0
	}
	if m.perItemWait > 0 && nodeQueue != nil {
		depthAhead := len(nodeQueue.pending)
		if nodeQueue.active != nil {
			depthAhead++
		}
		wait += time.Duration(depthAhead) * m.perItemWait
	}
	if m.maxWaitCap > 0 && wait > m.maxWaitCap {
		return m.maxWaitCap
	}
	return wait
}

func (m *HotplugQueueManager) snapshotLocked(node string) HotplugQueueNodeSnapshot {
	snapshot := HotplugQueueNodeSnapshot{Node: node}
	nodeQueue := m.nodes[node]
	if nodeQueue == nil {
		return snapshot
	}
	if nodeQueue.active != nil {
		active := requestSnapshot(nodeQueue.active)
		snapshot.Active = &active
		if !nodeQueue.active.startedAt.IsZero() {
			snapshot.ActiveAgeSeconds = int64(time.Since(nodeQueue.active.startedAt).Seconds())
		}
	}
	if len(nodeQueue.pending) > 0 {
		snapshot.Queued = make([]HotplugQueueItemSnapshot, 0, len(nodeQueue.pending))
		oldest := int64(0)
		now := time.Now()
		for _, req := range nodeQueue.pending {
			item := requestSnapshot(req)
			snapshot.Queued = append(snapshot.Queued, item)
			age := int64(now.Sub(req.enqueuedAt).Seconds())
			if age > oldest {
				oldest = age
			}
		}
		snapshot.QueuedCount = len(snapshot.Queued)
		snapshot.OldestAgeSeconds = oldest
	}
	return snapshot
}

func (m *HotplugQueueManager) recordDepthMetricsLocked() {
	if m == nil || m.metrics == nil {
		return
	}
	critical, normal, background := 0, 0, 0
	for _, nodeQueue := range m.nodes {
		if nodeQueue == nil {
			continue
		}
		if nodeQueue.active != nil {
			switch nodeQueue.active.priority {
			case hotplugQueuePriorityCritical:
				critical++
			case hotplugQueuePriorityNormal:
				normal++
			default:
				background++
			}
		}
		for _, req := range nodeQueue.pending {
			switch req.priority {
			case hotplugQueuePriorityCritical:
				critical++
			case hotplugQueuePriorityNormal:
				normal++
			default:
				background++
			}
		}
	}
	m.metrics.SetHotplugQueueDepth(string(hotplugQueuePriorityCritical), critical)
	m.metrics.SetHotplugQueueDepth(string(hotplugQueuePriorityNormal), normal)
	m.metrics.SetHotplugQueueDepth(string(hotplugQueuePriorityBackground), background)
}

func (m *HotplugQueueManager) persistSnapshot(snapshot HotplugQueueNodeSnapshot) {
	if m == nil || m.runtime == nil || !m.runtime.enabled || snapshot.Node == "" {
		return
	}
	if snapshot.Active == nil && snapshot.QueuedCount == 0 {
		m.persistSnapshotNow(snapshot)
		return
	}
	if m.snapshotDebounce > 0 {
		m.deferSnapshot(snapshot)
		return
	}
	m.persistSnapshotNow(snapshot)
}

func (m *HotplugQueueManager) deferSnapshot(snapshot HotplugQueueNodeSnapshot) {
	m.snapshotMu.Lock()
	if m.pendingSnapshots == nil {
		m.pendingSnapshots = map[string]HotplugQueueNodeSnapshot{}
	}
	if m.snapshotTimers == nil {
		m.snapshotTimers = map[string]*time.Timer{}
	}
	m.pendingSnapshots[snapshot.Node] = snapshot
	if _, exists := m.snapshotTimers[snapshot.Node]; !exists {
		node := snapshot.Node
		m.snapshotTimers[node] = time.AfterFunc(m.snapshotDebounce, func() {
			m.flushPendingSnapshot(node)
		})
	}
	m.snapshotMu.Unlock()
}

func (m *HotplugQueueManager) flushPendingSnapshot(node string) {
	m.snapshotMu.Lock()
	snapshot, ok := m.pendingSnapshots[node]
	delete(m.pendingSnapshots, node)
	delete(m.snapshotTimers, node)
	m.snapshotMu.Unlock()
	if ok {
		m.persistSnapshotNow(snapshot)
	}
}

func (m *HotplugQueueManager) persistSnapshotNow(snapshot HotplugQueueNodeSnapshot) {
	if m == nil || m.runtime == nil || !m.runtime.enabled || snapshot.Node == "" {
		return
	}
	m.snapshotMu.Lock()
	if timer := m.snapshotTimers[snapshot.Node]; timer != nil {
		timer.Stop()
		delete(m.snapshotTimers, snapshot.Node)
	}
	delete(m.pendingSnapshots, snapshot.Node)
	m.snapshotMu.Unlock()
	if snapshot.Active == nil && snapshot.QueuedCount == 0 {
		if err := m.runtime.DeleteConfigMapKey(context.Background(), m.namespace, hotplugQueueStateConfigMapName, snapshot.Node); err != nil {
			klog.V(4).InfoS("Failed to clear hotplug queue snapshot", "node", snapshot.Node, "err", err)
		}
		return
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		klog.V(4).InfoS("Failed to marshal hotplug queue snapshot", "node", snapshot.Node, "err", err)
		return
	}
	if err := m.runtime.UpsertConfigMapData(context.Background(), m.namespace, hotplugQueueStateConfigMapName, map[string]string{
		snapshot.Node: string(payload),
	}); err != nil {
		klog.V(4).InfoS("Failed to persist hotplug queue snapshot", "node", snapshot.Node, "err", err)
	}
}

func requestSnapshot(req *hotplugQueueRequest) HotplugQueueItemSnapshot {
	item := HotplugQueueItemSnapshot{
		ID:         req.id,
		Node:       req.node,
		Operation:  req.operation,
		Volume:     req.volume,
		Priority:   req.priority,
		EnqueuedAt: req.enqueuedAt,
		Coalesced:  req.coalesced,
	}
	if !req.startedAt.IsZero() {
		started := req.startedAt
		item.StartedAt = &started
	}
	return item
}

func (req *hotplugQueueRequest) complete(err error) {
	if req == nil {
		return
	}
	req.once.Do(func() {
		req.err = err
		close(req.done)
	})
}

func findDuplicateRequest(queue *hotplugNodeQueue, req *hotplugQueueRequest) *hotplugQueueRequest {
	if queue == nil || req == nil {
		return nil
	}
	if queue.active != nil && sameQueueRequest(queue.active, req) {
		return queue.active
	}
	for _, pending := range queue.pending {
		if sameQueueRequest(pending, req) {
			return pending
		}
	}
	return nil
}

func sameQueueRequest(a, b *hotplugQueueRequest) bool {
	return a != nil &&
		b != nil &&
		a.node == b.node &&
		a.operation == b.operation &&
		a.volume == b.volume
}
