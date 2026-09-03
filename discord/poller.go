package discord

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"nofx/logger"
	"nofx/store"
)

// MessageHandler processes one stored message for one subscriber (trader).
// isEdit marks a content revision of a previously seen message.
// Returning an error records the failure; the message is not retried
// automatically (subscribers deduplicate via their own signal records).
type MessageHandler func(msg *store.DiscordMessage, isEdit bool) error

// subscriber is one trader following a channel.
type subscriber struct {
	id      string
	handler MessageHandler
}

// channelWorker owns one channel: fetch bookkeeping + a serial dispatcher.
type channelWorker struct {
	channelID string
	subs      map[string]*subscriber
	kick      chan struct{} // wake the dispatcher
	stop      chan struct{}
	pausedTil time.Time
	lastError string
}

// PollerManager is the single Discord polling instance for the process.
//
// Architecture (correctness before throughput):
//   - ONE fetch loop serially polls all subscribed channels through the
//     rate-limited client and only upserts rows into discord_messages.
//     It is never blocked by AI or exchange work.
//   - Per channel, ONE dispatcher goroutine drains pending messages from the
//     durable store queue in message-timestamp order and runs all subscriber
//     handlers (different traders in parallel, each channel strictly serial).
//     Ordering per channel is what prevents a slow OPEN parse from being
//     overtaken by a later CLOSE.
//   - Restart safety: stuck "processing" rows are reset to pending at start;
//     handlers deduplicate via (trader, message, revision) signal records.
type PollerManager struct {
	store *store.Store

	mu       sync.Mutex
	client   *Client
	interval time.Duration
	enabled  bool
	workers  map[string]*channelWorker
	running  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewPollerManager creates the manager (call Start after wiring subscribers).
func NewPollerManager(st *store.Store) *PollerManager {
	return &PollerManager{
		store:   st,
		workers: make(map[string]*channelWorker),
	}
}

// ReloadConfig re-reads the global Discord config (token / interval / enabled).
// Safe to call at runtime; the next fetch cycle uses the new client.
func (pm *PollerManager) ReloadConfig() error {
	cfg, err := pm.store.DiscordConfig().Get()
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if err != nil {
		return err
	}
	if cfg == nil || string(cfg.Token) == "" {
		pm.client = nil
		pm.enabled = false
		return nil
	}
	pm.client = NewClient(string(cfg.Token))
	pm.enabled = cfg.Enabled
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval < 3*time.Second {
		interval = 3 * time.Second
	}
	pm.interval = interval
	return nil
}

// Client returns the current API client (nil when unconfigured).
// Used by API handlers for connection/channel tests and by the engine for
// linked-message fetches and media downloads.
func (pm *PollerManager) Client() *Client {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.client
}

// Start launches the fetch loop.
func (pm *PollerManager) Start() error {
	pm.mu.Lock()
	if pm.running {
		pm.mu.Unlock()
		return nil
	}
	pm.running = true
	pm.stopCh = make(chan struct{})
	pm.mu.Unlock()

	if _, err := pm.store.DiscordMessage().ResetStuckProcessing(); err != nil {
		logger.Warnf("[Discord] failed to reset stuck messages: %v", err)
	}
	if err := pm.ReloadConfig(); err != nil {
		logger.Warnf("[Discord] config load failed: %v", err)
	}

	pm.wg.Add(1)
	go pm.fetchLoop()
	logger.Infof("✅ Discord poller started")
	return nil
}

// Stop terminates the fetch loop and all dispatchers.
func (pm *PollerManager) Stop() {
	pm.mu.Lock()
	if !pm.running {
		pm.mu.Unlock()
		return
	}
	pm.running = false
	close(pm.stopCh)
	for _, w := range pm.workers {
		close(w.stop)
	}
	pm.workers = make(map[string]*channelWorker)
	pm.mu.Unlock()
	pm.wg.Wait()
}

// Subscribe registers a trader on a channel. The first subscriber of a fresh
// channel establishes a baseline: existing history is stored as skipped and
// never traded on.
func (pm *PollerManager) Subscribe(channelID, subscriberID string, handler MessageHandler) error {
	if channelID == "" || subscriberID == "" || handler == nil {
		return fmt.Errorf("invalid subscription")
	}

	pm.mu.Lock()
	w, exists := pm.workers[channelID]
	if !exists {
		w = &channelWorker{
			channelID: channelID,
			subs:      make(map[string]*subscriber),
			kick:      make(chan struct{}, 1),
			stop:      make(chan struct{}),
		}
		pm.workers[channelID] = w
		pm.wg.Add(1)
		go pm.dispatchLoop(w)
	}
	w.subs[subscriberID] = &subscriber{id: subscriberID, handler: handler}
	pm.mu.Unlock()

	logger.Infof("[Discord] subscriber %s attached to channel %s", subscriberID, channelID)
	return nil
}

// Unsubscribe detaches a trader; the channel stops polling when empty.
func (pm *PollerManager) Unsubscribe(channelID, subscriberID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	w, exists := pm.workers[channelID]
	if !exists {
		return
	}
	delete(w.subs, subscriberID)
	if len(w.subs) == 0 {
		close(w.stop)
		delete(pm.workers, channelID)
		logger.Infof("[Discord] channel %s has no subscribers, polling stopped", channelID)
	}
}

// fetchLoop serially polls every subscribed channel, sleeping the configured
// interval (with ±20% jitter) between cycles and a small gap between channels.
func (pm *PollerManager) fetchLoop() {
	defer pm.wg.Done()
	for {
		pm.mu.Lock()
		stopCh := pm.stopCh
		client := pm.client
		enabled := pm.enabled
		interval := pm.interval
		channels := make([]*channelWorker, 0, len(pm.workers))
		for _, w := range pm.workers {
			channels = append(channels, w)
		}
		pm.mu.Unlock()

		if interval <= 0 {
			interval = 6 * time.Second
		}

		if client != nil && enabled {
			for _, w := range channels {
				select {
				case <-stopCh:
					return
				default:
				}
				pm.fetchChannel(client, w)
				// Gentle gap between per-channel requests inside a cycle.
				time.Sleep(1500 * time.Millisecond)
			}
		}

		// Jittered cycle sleep: ±20% so the traffic pattern never looks robotic.
		jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(interval))
		select {
		case <-stopCh:
			return
		case <-time.After(interval + jitter):
		}
	}
}

// fetchChannel pulls the sliding window of one channel and persists changes.
// The window approach (latest 50, diff against store) is deliberate: jonzi
// trade cards deliver their whole lifecycle through in-place edits, which an
// `after=last_id` incremental fetch would miss entirely.
func (pm *PollerManager) fetchChannel(client *Client, w *channelWorker) {
	pm.mu.Lock()
	paused := time.Now().Before(w.pausedTil)
	pm.mu.Unlock()
	if paused {
		return
	}

	msgs, err := client.GetMessages(w.channelID, 50)
	if err != nil {
		pm.handleFetchError(w, err)
		return
	}

	msgStore := pm.store.DiscordMessage()
	// The explicit per-channel flag is the ONLY trusted baseline signal.
	// Row presence is not: cross-channel message lookups (reply/link context)
	// can persist single rows into channels that were never baselined.
	baselineDone, err := msgStore.IsBaselineDone(w.channelID)
	if err != nil {
		logger.Errorf("[Discord] channel %s baseline check failed: %v", w.channelID, err)
		return
	}

	// First contact: persist history as baseline, never dispatch it. The flag
	// is only set when EVERY message persisted; on partial failure the whole
	// baseline is retried next cycle (MarkBaseline is idempotent).
	if !baselineDone {
		allOK := true
		for _, m := range msgs {
			rec, convErr := ToStoreMessage(m, w.channelID)
			if convErr != nil {
				logger.Warnf("[Discord] baseline convert failed for %s: %v", m.ID, convErr)
				allOK = false
				continue
			}
			if blErr := msgStore.MarkBaseline(rec); blErr != nil {
				logger.Warnf("[Discord] baseline persist failed for %s: %v", m.ID, blErr)
				allOK = false
			}
		}
		if !allOK {
			logger.Warnf("[Discord] channel %s baseline incomplete, retrying next cycle", w.channelID)
			return
		}
		if doneErr := msgStore.MarkBaselineDone(w.channelID, len(msgs)); doneErr != nil {
			logger.Errorf("[Discord] channel %s baseline flag persist failed: %v", w.channelID, doneErr)
			return
		}
		logger.Infof("[Discord] channel %s baseline established (%d messages, not traded)", w.channelID, len(msgs))
		return
	}

	changed := 0
	// Oldest first so pending order matches message order.
	for i := len(msgs) - 1; i >= 0; i-- {
		rec, convErr := ToStoreMessage(msgs[i], w.channelID)
		if convErr != nil {
			logger.Warnf("[Discord] message %s convert failed: %v", msgs[i].ID, convErr)
			continue
		}
		result, upErr := msgStore.Upsert(rec)
		if upErr != nil {
			logger.Errorf("[Discord] message %s upsert failed: %v", rec.MessageID, upErr)
			continue
		}
		if result != store.DiscordMsgUnchanged {
			changed++
		}
	}

	if changed > 0 {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// handleFetchError pauses the channel appropriately: auth failures pause long
// (token invalid — operator action needed), transient errors pause briefly.
func (pm *PollerManager) handleFetchError(w *channelWorker, err error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	w.lastError = err.Error()
	switch {
	case IsAuthError(err):
		w.pausedTil = time.Now().Add(10 * time.Minute)
		logger.Errorf("[Discord] channel %s auth error (token invalid or no access), paused 10m: %v", w.channelID, err)
	case IsNotFound(err):
		w.pausedTil = time.Now().Add(30 * time.Minute)
		logger.Errorf("[Discord] channel %s not found, paused 30m: %v", w.channelID, err)
	default:
		w.pausedTil = time.Now().Add(1 * time.Minute)
		logger.Warnf("[Discord] channel %s fetch failed, paused 1m: %v", w.channelID, err)
	}
}

// dispatchLoop drains the durable queue of one channel in strict message
// order. Every message is delivered to all current subscribers (traders run
// in parallel per message; the channel itself stays serial).
func (pm *PollerManager) dispatchLoop(w *channelWorker) {
	defer pm.wg.Done()
	msgStore := pm.store.DiscordMessage()
	for {
		// Token cleared / polling disabled must also stop dispatching queued
		// messages: an operator disabling Discord expects NO further trading.
		pm.mu.Lock()
		active := pm.enabled && pm.client != nil
		pm.mu.Unlock()
		if !active {
			select {
			case <-w.stop:
				return
			case <-time.After(15 * time.Second):
				continue
			}
		}

		msg, err := msgStore.NextPending(w.channelID)
		if err != nil {
			logger.Errorf("[Discord] channel %s queue read failed: %v", w.channelID, err)
			msg = nil
		}
		if msg == nil {
			select {
			case <-w.stop:
				return
			case <-w.kick:
				continue
			case <-time.After(15 * time.Second):
				continue
			}
		}

		pm.mu.Lock()
		subs := make([]*subscriber, 0, len(w.subs))
		for _, s := range w.subs {
			subs = append(subs, s)
		}
		pm.mu.Unlock()

		isEdit := msg.Revision > 0
		var wg sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		for _, s := range subs {
			wg.Add(1)
			go func(s *subscriber) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						logger.Errorf("[Discord] handler %s panicked on message %s: %v", s.id, msg.MessageID, r)
						errMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("handler %s panic: %v", s.id, r)
						}
						errMu.Unlock()
					}
				}()
				if err := s.handler(msg, isEdit); err != nil {
					logger.Errorf("[Discord] handler %s failed on message %s: %v", s.id, msg.MessageID, err)
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}(s)
		}
		wg.Wait()

		status := store.DiscordMsgDone
		errText := ""
		if firstErr != nil {
			status = store.DiscordMsgFailed
			errText = firstErr.Error()
		}
		// Revision-conditional: if the author edited the message while we were
		// processing, the row is already pending with a bumped revision — the
		// terminal status of the OLD revision must not clobber it.
		applied, err := msgStore.SetStatusIfRevision(msg.ID, msg.Revision, status, errText)
		if err != nil {
			logger.Errorf("[Discord] message %s status update failed: %v", msg.MessageID, err)
		} else if !applied {
			logger.Infof("[Discord] message %s edited during processing; new revision stays pending for reprocessing", msg.MessageID)
		}

		select {
		case <-w.stop:
			return
		default:
		}
	}
}

// ChannelStatus is a lightweight health snapshot for the UI.
type ChannelStatus struct {
	ChannelID   string    `json:"channel_id"`
	Subscribers int       `json:"subscribers"`
	PausedUntil time.Time `json:"paused_until,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// Status reports current channel subscriptions.
func (pm *PollerManager) Status() []ChannelStatus {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]ChannelStatus, 0, len(pm.workers))
	for _, w := range pm.workers {
		out = append(out, ChannelStatus{
			ChannelID:   w.channelID,
			Subscribers: len(w.subs),
			PausedUntil: w.pausedTil,
			LastError:   w.lastError,
		})
	}
	return out
}
