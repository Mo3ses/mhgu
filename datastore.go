// datastore.go -- DataStore (protocol 0x73) handler for MHGU.
//
// WARNING: this is currently a STUB. It logs every method call and
// returns empty success, mirroring Splatoon 2's init_replay.go. But
// MHGU's online flow depends on DataStore far more heavily than
// Splatoon 2's:
//   - Gathering Hall lobbies are PersistentGatherings stored via
//     DataStore.
//   - Hunter profile (HR / hunter rank / village progress) round-trips
//     through DataStore in some form.
//   - Quest clear records and rewards are stored server-side.
//
// Until those handlers are implemented, the game will boot, accept
// connections, but fail to find or persist any of the above. The
// console will likely error out with "A communication error has
// occurred" the moment the player tries to enter the Hunter Hub.
//
// To go from stub to working:
//  1. Capture traffic with Ryujinx-Nextendo + tcpdump, filter for
//     protocol 0x73, run the full online flow.
//  2. Match captured method IDs against nex.DataStore's standard
//     constants (GetObject, UpdateObject, DeleteObject, GetSpecific,
//     etc. -- see nextendo-nex/types.go for the full list).
//  3. Implement the methods needed for Gathering Hall persistence
//     (likely: GetObject, UpdateObject, GetSpecific, CompleteObject).
//  4. Back the store with a real DB (Postgres recommended -- the
//     in-memory variant dies on every restart, and MHGU sessions can
//     last hours).

package main

import (
	"log/slog"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

// setupMHGUDataStore installs a DataStore (0x73) stub on ep. Every
// method is answered with empty success + a log line.
func setupMHGUDataStore(ep *nex.Endpoint) {
	ep.Register(0x73, datastoreStubHandler())
	logger.Warn("DataStore handler is a STUB -- Gathering Hall persistence is not implemented")
}

// datastoreStubHandler returns an RMCHandler that answers every method
// with empty success and a one-line log. Wire-compatible with what the
// game expects from a server it just connected to, but semantically
// useless -- every read returns nothing, every write is dropped.
func datastoreStubHandler() nex.RMCHandler {
	return func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
		s := conn.Settings
		logger.Debug("datastore stub",
			slog.Uint64("pid", conn.PID),
			slog.Uint32("method", req.Method),
			slog.Int("bodyLen", len(req.Body)))
		return nex.NewRMCSuccess(s, 0x73, req.Method, req.CallID, nil)
	}
}