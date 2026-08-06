// mhgu_utility.go -- wrapper around nex.UtilityHandler for MHGU.
//
// At the time of this scaffold we don't yet know which (if any)
// MHGU-specific Utility methods exist beyond the core's set. Splatoon 2
// added two (UpdateCurrentUser / AcquireTagId); SSBU added a different
// pair. MHGU's are TBD.
//
// The wrapper exists so that when we discover them (via traffic
// capture or further RE), the addition is local to this file and does
// not require touching main.go.
//
// CRITICAL: the default branch MUST delegate to the base handler.
// Splatoon 2's earlier mistake was answering only its own methods and
// returning NotImplemented otherwise -- which silently hid the core's
// AcquireNexUniqueIDWithPassword (0x6E.2) and broke fresh-account
// logins. See splatoon-2/utility.go:50-55.

package main

import (
	"log/slog"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

// Known method IDs we may want to override in the future. Listed here
// so they're discoverable; not used yet.
const (
	// Core Utility method that must always reach the base handler.
	methodUtilityAcquireNexUniqueIDWithPassword uint32 = 2

	// Splatoon 2's custom methods -- kept as comments for reference.
	// methodUtilityUpdateCurrentUser uint32 = 10
	// methodUtilityAcquireTagID       uint32 = 9
)

// setupMHGUUtility installs the Utility-handler wrapper on ep. Until
// MHGU-specific methods are discovered, this is a passthrough.
func setupMHGUUtility(ep *nex.Endpoint) {
	base := nex.UtilityHandler()

	ep.Register(nex.ProtocolUtility,
		func(conn *nex.Connection, req *nex.RMCMessage) *nex.RMCMessage {
			s := conn.Settings
			switch req.Method {
			// Future MHGU overrides go here. Until then, the default
			// branch reaches the core handler, which routes every
			// other method correctly (including
			// AcquireNexUniqueIDWithPassword for fresh accounts).
			default:
				return base(conn, req)
			}
		})

	logger.Info("MHGU utility handler registered",
		slog.String("proto", "0x6e"))
}