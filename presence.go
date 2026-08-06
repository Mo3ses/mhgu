// presence.go -- "playing MHGU" presence reporter.
//
// Watches the secure endpoint for active PIDs (via notePresenceSeen, fed
// from OnRMC in main.go) and batches a POST to nextendo-account every
// NEXTENDO_PRESENCE_INTERVAL. The account server uses this to display
// "playing Monster Hunter Generations Ultimate" in the user profile
// UI and to enforce the per-game "playing X elsewhere" gate.
//
// Pattern matches splatoon-2/presence.go verbatim, with mhguAppID
// instead of the Splatoon 2 app ID and slog instead of fmt.Printf.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

// presenceTTL keeps stale entries from outliving a crashed-then-
// reconnected client.
const (
	presenceTTL  = 60 * time.Second
	defaultFlush = 30 * time.Second
)

var (
	presenceMu  sync.Mutex
	presenceMap = map[uint64]time.Time{}
)

// notePresenceSeen records that pid is currently playing MHGU. Called
// from the secure endpoint's OnRMC hook -- any inbound packet proves
// the account is online and playing this title.
func notePresenceSeen(pid uint64) {
	if pid == 0 {
		return
	}
	presenceMu.Lock()
	presenceMap[pid] = time.Now()
	presenceMu.Unlock()
}

// presenceBatch is the JSON shape POSTed to
// nextendo-account /internal/presence.
type presenceBatch struct {
	AppID string   `json:"app_id"`
	PIDs  []uint64 `json:"pids"`
}

// startPresenceReporter launches the background flush goroutine and
// returns a stop function the caller defers at shutdown.
//
// The goroutine exits cleanly when stop() is called or the context is
// cancelled (e.g. by signal.NotifyContext during shutdown).
func startPresenceReporter(_ *nex.Endpoint) func() {
	interval := envOrDuration(envPresenceInterval, defaultFlush)
	if interval <= 0 {
		interval = defaultFlush
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				flushPresence()
			}
		}
	}()

	logger.Info("presence reporter started",
		slog.Duration("interval", interval))
	return func() {
		cancel()
		<-done
		flushPresence() // one last drain so in-flight PIDs get reported.
		logger.Info("presence reporter stopped")
	}
}

// flushPresence collects every PID seen in the last presenceTTL and
// POSTs them in one batch. Failures are logged and swallowed -- a
// missing presence ping must never crash the server.
func flushPresence() {
	now := time.Now()
	cutoff := now.Add(-presenceTTL)

	presenceMu.Lock()
	pids := make([]uint64, 0, len(presenceMap))
	for pid, ts := range presenceMap {
		if ts.Before(cutoff) {
			delete(presenceMap, pid)
			continue
		}
		pids = append(pids, pid)
	}
	presenceMu.Unlock()

	if len(pids) == 0 || accountBaseURL == "" {
		return
	}

	batch := presenceBatch{AppID: mhguAppID, PIDs: pids}
	body, err := json.Marshal(batch)
	if err != nil {
		logger.Warn("presence: marshal failed", slog.Any("err", err))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		accountBaseURL+"/internal/presence", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", internalKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Debug("presence: POST failed", slog.Any("err", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		logger.Debug("presence: non-2xx",
			slog.Int("status", resp.StatusCode),
			slog.Int("pids", len(pids)))
	}
}

// envOrDuration returns the env var parsed as a time.Duration, or def
// on parse failure / unset. Accepts forms like "30s", "1m", "2m30s".
func envOrDuration(key string, def time.Duration) time.Duration {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

// compile-time guard
var _ = envOrDuration("", 0)