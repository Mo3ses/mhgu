// dashboard.go -- monitoring dashboard for the MHGU server.
//
// Listens on DASH_PORT (default 8085) and exposes:
//
//	GET /healthz                 -- unauthenticated "ok"
//	GET /api/stats?key=<token>   -- JSON stats blob
//	GET /api/kick?key=&pid=      -- evict a stuck account
//
// Improvements over splatoon-2/dashboard.go:
//   - MHGU labels (max 4 hunters, not 12 racers)
//   - slog instead of fmt.Printf
//   - Constant-time token comparison (already in splatoon-2; kept)
//   - SNIHost reads from nextendoHost, not the empty constant
//
// The metrics tracked are:
//   - Total RMC calls, broken down by (protocol, method)
//   - Peak concurrent connections
//   - Per-player meta: connect time, last RMC, gather memberships
//   - 100-entry ring of recent events (connect / disconnect / kick /
//     notImplemented), for quick visual debugging during RE work.

package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

const (
	dashEventRingSize = 100
	// maxHunters is the size of an MHGU lobby. Used as a sanity check
	// on dashboard data; not enforced on the wire.
	maxHunters = 4
)

// playerMeta holds per-PID information for the dashboard.
type playerMeta struct {
	PID         uint64    `json:"pid"`
	ConnectedAt time.Time `json:"connected_at"`
	LastSeen    time.Time `json:"last_seen"`
	RMCCount    uint64    `json:"rmc_count"`
}

// event is a single dashboard event.
type event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	PID    uint64    `json:"pid,omitempty"`
	Proto  string    `json:"proto,omitempty"`
	Method uint32    `json:"method,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// methodCounter tracks call counts per (protocol, method).
type methodCounter struct {
	Proto  uint16 `json:"proto"`
	Method uint32 `json:"method"`
	Name   string `json:"name"`
	Count  uint64 `json:"count"`
}

// apiStats is the JSON shape returned by /api/stats.
type apiStats struct {
	Uptime        string          `json:"uptime"`
	Connected     int             `json:"connected"`
	PeakConnected uint64          `json:"peak_connected"`
	TotalRMC      uint64          `json:"total_rmc"`
	ActiveLobbies int             `json:"active_lobbies"`
	Players       []*playerMeta   `json:"players"`
	RecentEvents  []event         `json:"recent_events"`
	TopMethods    []methodCounter `json:"top_methods"`
	ServerInfo    map[string]any  `json:"server_info"`
}

// dashState holds the dashboard's mutable state.
type dashState struct {
	mu            sync.RWMutex
	connected     atomic.Int64
	peakConnected atomic.Uint64
	totalRMC      atomic.Uint64
	players       map[uint64]*playerMeta
	methodCounts  map[uint16]map[uint32]*methodCounter // proto -> method -> counter
	events        []event
	startedAt     time.Time
}

var dash = &dashState{
	players:      map[uint64]*playerMeta{},
	methodCounts: map[uint16]map[uint32]*methodCounter{},
	startedAt:    time.Now(),
}

// noteRMC records an RMC call for the dashboard counters. Called from
// the secure OnRMC hook in main.go.
func noteRMC(c *nex.Connection, req *nex.RMCMessage) {
	dash.totalRMC.Add(1)

	dash.mu.Lock()
	defer dash.mu.Unlock()

	if _, ok := dash.methodCounts[req.Protocol]; !ok {
		dash.methodCounts[req.Protocol] = map[uint32]*methodCounter{}
	}
	mc, ok := dash.methodCounts[req.Protocol][req.Method]
	if !ok {
		mc = &methodCounter{
			Proto:  req.Protocol,
			Method: req.Method,
			Name:   methodName(req.Protocol, req.Method),
		}
		dash.methodCounts[req.Protocol][req.Method] = mc
	}
	mc.Count++

	if pm, ok := dash.players[c.PID]; ok {
		pm.LastSeen = time.Now()
		pm.RMCCount++
	}
}

// noteEvent appends an event to the ring buffer.
func noteEvent(kind string, c *nex.Connection, req *nex.RMCMessage, note string) {
	dash.mu.Lock()
	defer dash.mu.Unlock()

	e := event{
		Time: time.Now(),
		Kind: kind,
		Note: note,
	}
	if c != nil {
		e.PID = c.PID
	}
	if req != nil {
		e.Proto = fmt.Sprintf("0x%x", req.Protocol)
		e.Method = req.Method
	}
	dash.events = append(dash.events, e)
	if len(dash.events) > dashEventRingSize {
		dash.events = dash.events[len(dash.events)-dashEventRingSize:]
	}
}

// methodName returns a human-readable name for a (protocol, method)
// pair, falling back to "M<method>" for unknowns.
func methodName(proto uint16, method uint32) string {
	// Core protocols.
	switch {
	case proto == nex.ProtocolSecureConnection:
		switch method {
		case 1:
			return "Register"
		case 2:
			return "RegisterEx"
		case 7:
			return "ReplaceURL"
		}
	case proto == nex.ProtocolTicketGranting:
		switch method {
		case 1:
			return "Login"
		case 2:
			return "LoginEx"
		}
	case proto == nex.ProtocolMatchmakeExtension:
		switch method {
		case MethodUpdateFriendUserProfile:
			return "MHXX.UpdateFriendUserProfile"
		case MethodGetFriendUserProfiles:
			return "MHXX.GetFriendUserProfiles"
		case MethodGetFriends:
			return "MHXX.GetFriends"
		case MethodAddFriends:
			return "MHXX.AddFriends"
		case MethodRemoveFriend:
			return "MHXX.RemoveFriend"
		case MethodFindCommunityByOwner:
			return "MHXX.FindCommunityByOwner"
		}
	case proto == nex.ProtocolUtility:
		switch method {
		case 1:
			return "AcquireNexUniqueID"
		case 2:
			return "AcquireNexUniqueIDWithPassword"
		case 3:
			return "PingDaemon"
		}
	}
	return fmt.Sprintf("M%d", method)
}

// startDashboard launches the monitoring HTTP server in the background
// and returns its *http.Server so the caller can shut it down on
// signal.
func startDashboard(ep *nex.Endpoint, _ *nex.Matchmaking) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/stats", dashStatsHandler)
	mux.HandleFunc("/api/kick", dashKickHandler(ep))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", dashPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("dashboard listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("dashboard died", slog.Any("err", err))
		}
	}()

	// Connect/disconnect observers. We poll SnapshotConnections every
	// 2s as a cheap substitute for explicit OnConnect/OnDisconnect
	// hooks (which are already wired in main.go for the OnDisconnect
	// leak fix). This adds a "connect" event to the ring without
	// having to thread noteEvent through main.go twice.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var lastConnected int
		for range ticker.C {
			conns := ep.SnapshotConnections()
			dash.connected.Store(int64(len(conns)))
			if uint64(len(conns)) > dash.peakConnected.Load() {
				dash.peakConnected.Store(uint64(len(conns)))
			}
			if len(conns) > lastConnected {
				for _, c := range conns {
					dash.mu.Lock()
					if _, ok := dash.players[c.PID]; !ok {
						dash.players[c.PID] = &playerMeta{
							PID:         c.PID,
							ConnectedAt: time.Now(),
							LastSeen:    time.Now(),
						}
						noteEvent("connect", nil, nil, "")
					}
					dash.mu.Unlock()
				}
			}
			lastConnected = len(conns)
		}
	}()

	return srv
}

// dashStatsHandler returns the JSON stats blob. Requires ?key= if
// DASH_TOKEN is set; uses constant-time comparison.
func dashStatsHandler(w http.ResponseWriter, r *http.Request) {
	if !authDash(w, r) {
		return
	}

	dash.mu.RLock()
	players := make([]*playerMeta, 0, len(dash.players))
	for _, pm := range dash.players {
		players = append(players, pm)
	}
	events := append([]event(nil), dash.events...)
	methods := make([]methodCounter, 0)
	for _, byMethod := range dash.methodCounts {
		for _, mc := range byMethod {
			methods = append(methods, *mc)
		}
	}
	dash.mu.RUnlock()

	stats := apiStats{
		Uptime:        time.Since(dash.startedAt).Round(time.Second).String(),
		Connected:     int(dash.connected.Load()),
		PeakConnected: dash.peakConnected.Load(),
		TotalRMC:      dash.totalRMC.Load(),
		Players:       players,
		RecentEvents:  events,
		TopMethods:    methods,
		ServerInfo: map[string]any{
			"app_id":      mhguAppID,
			"host":        nextendoHost,
			"auth_port":   authPort,
			"secure_port": securePort,
			"max_hunters": maxHunters,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(stats)
}

// dashKickHandler evicts a stuck account by PID. Requires ?key= and
// ?pid=<number>.
func dashKickHandler(ep *nex.Endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authDash(w, r) {
			return
		}
		pidStr := r.URL.Query().Get("pid")
		pid, err := strconv.ParseUint(pidStr, 10, 64)
		if err != nil || pid == 0 {
			http.Error(w, "bad pid", http.StatusBadRequest)
			return
		}
		kicked := ep.KickPID(pid)
		noteEvent("kick", nil, nil, fmt.Sprintf("pid=%d kicked=%d", pid, kicked))
		fmt.Fprintf(w, "kicked %d connections\n", kicked)
	}
}

// authDash closes the request with 403 if the token doesn't match.
// Uses constant-time comparison to avoid leaking the correct-prefix
// length.
func authDash(w http.ResponseWriter, r *http.Request) bool {
	if dashToken == "" {
		// Empty token = open access. Documented as a foot-gun in the
		// README. Production deployments MUST set DASH_TOKEN.
		return true
	}
	got := r.URL.Query().Get("key")
	if subtle.ConstantTimeCompare([]byte(got), []byte(dashToken)) == 1 {
		return true
	}
	http.Error(w, "forbidden", http.StatusForbidden)
	return false
}