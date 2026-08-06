// mhgu_matchmake.go -- MHXX Matchmake Extension Protocol (protocol 0x6D /
// ID 109) for Monster Hunter Generations Ultimate.
//
// Source for the six documented methods:
// https://github.com/kinnay/NintendoClients/wiki/Matchmake-Extension-Protocol-(MHXX)
//
// The MHXX protocol reuses the standard Matchmake Extension protocol
// slot (nex.ProtocolMatchmakeExtension == 0x6D == 109 decimal), the
// same one Animal Crossing: New Horizons uses (see
// matchmaking_acnh.go in nextendo-nex). The six methods here are
// specific to MHGU -- friend profile, friends list, and the Gathering
// Hall community search.
//
// State is in-memory only (dies on restart). Friends and gathering
// persistence should be backed by nextendo-account or a real DB once
// the access key + ACID are filled in.

package main

import (
	"log/slog"
	"sync"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

// MHXX Matchmake Extension Protocol method IDs (kinnay wiki).
const (
	MethodUpdateFriendUserProfile uint32 = 54
	MethodGetFriendUserProfiles   uint32 = 55
	MethodGetFriends              uint32 = 56
	MethodAddFriends              uint32 = 57
	MethodRemoveFriend            uint32 = 58
	MethodFindCommunityByOwner    uint32 = 59
)

// FriendUserParam { String name } -- body of method 54 (Update).
type FriendUserParam struct {
	Name string
}

func (p *FriendUserParam) Levels() []nex.Level {
	return []nex.Level{{
		Version: 0,
		Save:    func(o *nex.StreamOut) { o.String(p.Name) },
		Load:    func(i *nex.StreamIn) { p.Name = i.String() },
	}}
}

// FriendUserInfo { Uint64 pid; String name; Uint32 presence } -- element
// of List returned by methods 55 and 56.
type FriendUserInfo struct {
	PID      uint64
	Name     string
	Presence uint32
}

func (f *FriendUserInfo) Levels() []nex.Level {
	return []nex.Level{{
		Version: 0,
		Save: func(o *nex.StreamOut) {
			o.U64(f.PID)
			o.String(f.Name)
			o.U32(f.Presence)
		},
		Load: func(i *nex.StreamIn) {
			f.PID = i.U64()
			f.Name = i.String()
			f.Presence = i.U32()
		},
	}}
}

// MHXXStore holds the per-process MHXX state. Concurrency: guarded by
// mu. Memory: dies with the process -- swap for a real DB before
// running in production.
type MHXXStore struct {
	mu           sync.Mutex
	friendsByPID map[uint64][]uint64 // pid -> friend PIDs
	profileByPID map[uint64]string   // pid -> last-reported display name

	// TODO: community / persistent gathering index by owner PID.
	// Requires PersistentGathering from nextendo-nex + the MHGU-specific
	// `attributes` blob layout, which is undocumented publicly. See
	// README.md -> "What's missing".
}

// NewMHXXStore returns an empty store. Called once from
// setupMHGUMatchmake.
func NewMHXXStore() *MHXXStore {
	return &MHXXStore{
		friendsByPID: map[uint64][]uint64{},
		profileByPID: map[uint64]string{},
	}
}

// setupMHGUMatchmake wraps the core MatchmakeExtension handler with the
// six MHXX-specific methods (54-59). It registers the wrapper on ep,
// OVERRIDING any previous handler for protocol 0x6D. Call this INSTEAD
// of ep.Register(nex.ProtocolMatchmakeExtension, mm.ExtensionHandler()).
//
// The default branch delegates to the core handler -- which itself
// falls back to nex.notImplemented for methods that core doesn't
// recognize -- so unknown MHXX methods surface in the log with the
// full pid/method/bodyLen diagnostic.
func setupMHGUMatchmake(ep *nex.Endpoint, mm *nex.Matchmaking) {
	base := mm.ExtensionHandler()
	store := NewMHXXStore()

	ep.Register(nex.ProtocolMatchmakeExtension,
		func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
			switch req.Method {
			case MethodUpdateFriendUserProfile:
				return handleUpdateFriendUserProfile(conn, req, store)
			case MethodGetFriendUserProfiles:
				return handleGetFriendUserProfiles(conn, req, store)
			case MethodGetFriends:
				return handleGetFriends(conn, req, store)
			case MethodAddFriends:
				return handleAddFriends(conn, req, store)
			case MethodRemoveFriend:
				return handleRemoveFriend(conn, req, store)
			case MethodFindCommunityByOwner:
				return handleFindCommunityByOwner(conn, req, store)
			default:
				return base(conn, req)
			}
		})

	logger.Info("MHXX matchmake extension registered",
		slog.String("proto", "0x6d"),
		slog.Int("mhxx_methods", 6))
}

// --- Method handlers ----------------------------------------------------

// handleUpdateFriendUserProfile -- method 54.
//
// Request body: FriendUserParam { String name }
// Response: empty success.
//
// Records the calling PID's display name. We don't currently push it
// back to nextendo-account; that's a TODO.
func handleUpdateFriendUserProfile(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings
	in := nex.NewStreamIn(req.Body, s)
	param := &FriendUserParam{}
	param.Levels()[0].Load(in)

	store.mu.Lock()
	store.profileByPID[conn.PID] = param.Name
	store.mu.Unlock()

	logger.Debug("mhxx UpdateFriendUserProfile",
		slog.Uint64("pid", conn.PID),
		slog.String("name", param.Name))
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, nil)
}

// handleGetFriendUserProfiles -- method 55.
//
// Request body: List<Uint64> pids
// Response body: List<FriendUserInfo> (one per requested PID that we
// know about).
func handleGetFriendUserProfiles(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings
	in := nex.NewStreamIn(req.Body, s)
	pids := nex.ReadList(in, func(i *nex.StreamIn) uint64 { return i.U64() })

	store.mu.Lock()
	out := nex.NewStreamOut(s)
	results := make([]FriendUserInfo, 0, len(pids))
	for _, pid := range pids {
		name, ok := store.profileByPID[pid]
		if !ok {
			name = ""
		}
		results = append(results, FriendUserInfo{
			PID:      pid,
			Name:     name,
			Presence: 0, // TODO: pull from presenceMap
		})
	}
	store.mu.Unlock()

	nex.WriteList(out, results, func(o *nex.StreamOut, f FriendUserInfo) {
		o.Add(&f)
	})
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, out.Bytes())
}

// handleGetFriends -- method 56.
//
// Request body: empty
// Response body: List<FriendUserInfo> -- the calling PID's friends.
func handleGetFriends(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings

	store.mu.Lock()
	friendPIDs := store.friendsByPID[conn.PID]
	results := make([]FriendUserInfo, 0, len(friendPIDs))
	for _, pid := range friendPIDs {
		name := store.profileByPID[pid]
		results = append(results, FriendUserInfo{
			PID:      pid,
			Name:     name,
			Presence: 0,
		})
	}
	store.mu.Unlock()

	out := nex.NewStreamOut(s)
	nex.WriteList(out, results, func(o *nex.StreamOut, f FriendUserInfo) {
		o.Add(&f)
	})
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, out.Bytes())
}

// handleAddFriends -- method 57.
//
// Request body: List<Uint64> pids
// Response body: empty success.
//
// Appends to the caller's friend list (idempotent).
func handleAddFriends(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings
	in := nex.NewStreamIn(req.Body, s)
	add := nex.ReadList(in, func(i *nex.StreamIn) uint64 { return i.U64() })

	store.mu.Lock()
	existing := store.friendsByPID[conn.PID]
	have := make(map[uint64]struct{}, len(existing))
	for _, p := range existing {
		have[p] = struct{}{}
	}
	for _, p := range add {
		if _, ok := have[p]; ok {
			continue
		}
		existing = append(existing, p)
		have[p] = struct{}{}
	}
	store.friendsByPID[conn.PID] = existing
	store.mu.Unlock()

	logger.Debug("mhxx AddFriends",
		slog.Uint64("pid", conn.PID),
		slog.Int("added", len(add)))
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, nil)
}

// handleRemoveFriend -- method 58.
//
// Request body: Uint64 pid
// Response body: empty success.
func handleRemoveFriend(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings
	in := nex.NewStreamIn(req.Body, s)
	target := in.U64()

	store.mu.Lock()
	existing := store.friendsByPID[conn.PID]
	out := existing[:0]
	for _, p := range existing {
		if p != target {
			out = append(out, p)
		}
	}
	store.friendsByPID[conn.PID] = out
	store.mu.Unlock()

	logger.Debug("mhxx RemoveFriend",
		slog.Uint64("pid", conn.PID),
		slog.Uint64("target", target))
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, nil)
}

// handleFindCommunityByOwner -- method 59.
//
// Request body: Uint64 ownerPID, ResultRange resultRange
// Response body: List<PersistentGathering> -- the player's owned
// gathering halls (i.e. Gathering Hall lobbies).
//
// STUB: returns an empty list. The real implementation needs:
//   - The PersistentGathering structure layout (already in nextendo-nex
//     as MatchmakeSession with persistent flag set), and
//   - The MHGU-specific `attributes` blob format, which is undocumented
//     and must come from traffic capture.
func handleFindCommunityByOwner(conn *nex.Connection, req *nex.RMCMessage, store *MHXXStore) *nex.RMCMessage {
	s := conn.Settings
	in := nex.NewStreamIn(req.Body, s)
	ownerPID := in.U64()
	// resultRange: { offset, size } -- skip the body for now, we don't
	// honour pagination yet.
	_ = in.U32() // offset
	_ = in.U32() // size

	logger.Debug("mhxx FindCommunityByOwner (stub)",
		slog.Uint64("pid", conn.PID),
		slog.Uint64("owner", ownerPID))

	out := nex.NewStreamOut(s)
	out.U32(0) // empty list
	return nex.NewRMCSuccess(s, nex.ProtocolMatchmakeExtension, req.Method, req.CallID, out.Bytes())
}

// compile-time guard: make sure the structures actually satisfy nex.Structure.
var (
	_ nex.Structure = (*FriendUserParam)(nil)
	_ nex.Structure = (*FriendUserInfo)(nil)
)