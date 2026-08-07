// gates.go -- HTTP integration with nextendo-account.
//
// Two gates:
//
//	nextendoOnlineCheck(pid, kind)  ->  allow|disabled|unverified|elsewhere
//	resolveNSAtoPID(nsa)            ->  pid, nsaStatus
//
// Online-check is FAIL-OPEN (a transient account-server hiccup must
// never lock everyone out of MHGU). NSA resolution is FAIL-CLOSED (a
// failed lookup must never let an unauthenticated client through).
//
// Both are the established Nextendo pattern -- see splatoon-2/gates.go
// for the prior art. Behavior here is intentionally identical.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// accountBaseURL is the configured nextendo-account base URL.
var (
	accountBaseURL string
	internalKey    string
	httpClient     = &http.Client{Timeout: 3 * time.Second}
)

// nsaStatus describes the outcome of resolveNSAtoPID.
type nsaStatus int

const (
	nsaOK nsaStatus = iota
	nsaUnknown      // account server said 404 -- the NSA ID is not linked
	nsaUnreachable  // network/5xx -- treat as deny
	nsaInvalid      // bad input format
)

// reason strings returned by nextendoOnlineCheck. Matches the JSON
// field `reason` produced by nextendo-account's /internal/online-check
// route.
const (
	reasonUnknown    = "unknown"
	reasonDisabled   = "disabled"
	reasonUnverified = "unverified"
	reasonElsewhere  = "elsewhere"
)

// onlineCheckResult mirrors the JSON body of nextendo-account's response.
type onlineCheckResult struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

// nextendoOnlineCheck returns (allow, reason) for whether the given PID
// is allowed to play MHGU right now.
//
// Failure modes:
//   - Network error / non-2xx -> (true, "unknown"). FAIL-OPEN.
//   - HTTP 200 with allow:false -> (false, reason).
//
// "kind" is the game slug (e.g. "mhgu") -- nextendo-account uses it
// for the per-game "playing X elsewhere" gate.
func nextendoOnlineCheck(pid uint64, kind string) (bool, string) {
	if accountBaseURL == "" {
		return true, reasonUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	body := fmt.Sprintf(`{"pid":%d,"kind":%q}`, pid, kind)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		accountBaseURL+"/internal/online-check", strings.NewReader(body))
	if err != nil {
		logger.Warn("online-check: build request failed", slog.Any("err", err))
		return true, reasonUnknown
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Key", internalKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Warn("online-check: request failed", slog.Any("err", err))
		return true, reasonUnknown
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Warn("online-check: non-200", slog.Int("status", resp.StatusCode))
		return true, reasonUnknown
	}

	var out onlineCheckResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		logger.Warn("online-check: decode failed", slog.Any("err", err))
		return true, reasonUnknown
	}
	if !out.Allow {
		logger.Debug("online-check denied",
			slog.Uint64("pid", pid),
			slog.String("reason", out.Reason))
	}
	return out.Allow, out.Reason
}

// nsaCache maps an NSA baasUserID -> resolved PID. The cache is never
// invalidated -- a re-link requires a process restart.
var (
	nsaCacheMu sync.RWMutex
	nsaCache   = map[string]uint64{}
)

// nsaResolveResult is the JSON shape returned by
// nextendo-account's /api/nsa endpoint.
type nsaResolveResult struct {
	PID uint64 `json:"pid"`
}

// resolveNSAtoPID resolves a Nintendo Switch Account baasUserID (NSA) to
// a Nextendo PID via nextendo-account. Always fail-closed: a 404
// returns nsaUnknown (so the caller should reject), a network error
// returns nsaUnreachable (same outcome -- but the reason is logged so
// the operator can tell them apart).
//
// Cached forever in-process. To re-link after an account migration you
// must restart the server.
func resolveNSAtoPID(nsa string) (uint64, nsaStatus) {
	if nsa == "" {
		return 0, nsaInvalid
	}

	nsaCacheMu.RLock()
	if pid, ok := nsaCache[nsa]; ok {
		nsaCacheMu.RUnlock()
		return pid, nsaOK
	}
	nsaCacheMu.RUnlock()

	if accountBaseURL == "" {
		return 0, nsaUnreachable
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	u := accountBaseURL + "/api/nsa?id=" + url.QueryEscape(nsa)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nsaUnreachable
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Warn("nsa resolve: request failed", slog.Any("err", err))
		return 0, nsaUnreachable
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out nsaResolveResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			logger.Warn("nsa resolve: decode failed", slog.Any("err", err))
			return 0, nsaUnreachable
		}
		nsaCacheMu.Lock()
		nsaCache[nsa] = out.PID
		nsaCacheMu.Unlock()
		return out.PID, nsaOK
	case http.StatusNotFound:
		return 0, nsaUnknown
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		logger.Warn("nsa resolve: unexpected status",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)))
		return 0, nsaUnreachable
	}
}

// loadGatesConfig fills accountBaseURL / internalKey from env. Called
// from main() AFTER setupLogger() so the package-level logger exists
// for any warning we want to emit. (A previous version used a Go init()
// function, but init() runs before main() and so before setupLogger();
// that path panicked with nil-pointer dereference.)
func loadGatesConfig() {
	accountBaseURL = envOr(envAccountURL, "")
	internalKey = envOr(envInternalKey, "")
	if accountBaseURL == "" {
		logger.Warn("NEXTENDO_ACCOUNT_URL unset -- gates are no-ops")
	}
}
