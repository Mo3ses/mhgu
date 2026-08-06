# nextendo-mhgu

**Monster Hunter Generations Ultimate game server for the Nextendo Network.**

A NEX/PRUDP game server for MHGU (Switch, 2018), built on top of
[`nextendo-nex`](https://github.com/NextendoNetwork/nextendo-nex). Sibling of
[`splatoon-2`](https://github.com/NextendoNetwork/splatoon-2) and
[`mario-kart-8-deluxe`](https://github.com/NextendoNetwork/mario-kart-8-deluxe).

## Status: scaffold only

This repository is the first public attempt at an MHGU NEX server. It currently
contains:

- A working `main.go` that wires the standard Nextendo game-server layout.
- Stub implementations of the **six publicly documented** methods of the MHXX
  Matchmake Extension Protocol (protocol ID 109, methods 54-59) — see
  `mhgu_matchmake.go`.
- Standard handlers from `nextendo-nex`: SecureConnection, MatchMaking,
  MatchMakingExt, MatchmakeExtension, NATTraversal, Utility, Ranking,
  DataStore (stub), Notifications (push-only).
- Account integration (`gates.go`), presence reporting, and a dashboard.

**It does NOT yet have the MHGU ACID, access key, or the game-specific
quest/lobby protocol methods.** Until those are filled in (see "What's
missing" below), the server boots and accepts PRUDP handshakes but the
console will not progress past initial matchmaking.

## What's missing

These are the three blocking unknowns that require reverse-engineering work
on the MHGU binary:

| Unknown | Where to find it | Status |
| --- | --- | --- |
| **Access key** (per-title 8-hex string) | MHGU NSP -> `code.bin` / main NSO. Nextendo's `mario-kart-8-deluxe` README documents how to derive it. | TODO |
| **NEX version** (e.g. `40000` for MK8D) | Same binary, near the NEX initialization. May not be `40000`. | TODO |
| **MHGU ACID** (4-byte hex game-server ID) | MHGU NSP / hardcoded URL fragment like `g{ACID}-lp1.s.n.srv.nintendo.net`. | TODO |
| **Pia config** (`LegacyPiaConfig` vs `SwitchPia519Config`) | Empirically, via traffic capture. MHGU shipped 2018-08, between S2 (2017) and SSBU (2018-12). | TODO |
| **Station scheme** (`prudp` vs `prudps`) | Same capture. | TODO |
| **PRUDP minor version** (0 vs 5) | Same capture. | TODO |
| **Game-specific RMC methods** (quest dispatch, party state, Palico, lobby chat) | Traffic capture via Ryujinx-Nextendo on PC, or on a hacked Switch via `nx-hbmenu` + `mitmproxy`. | TODO |
| **GatheringHall state machine** | Reverse-engineering of `PersistentGathering.attributes` blob layout. | TODO |

The capture setup the existing community recommends is to run MHGU inside
`Ryujinx-Nextendo` (the Nextendo-flavored fork) with a logging shim on the
PRUDP socket, perform the full online flow (create hall -> post quest ->
accept -> in-quest -> return -> complete), and capture every packet. See
[kinnay/NintendoClients wiki](https://github.com/kinnay/NintendoClients/wiki)
for the only public reference to the MHXX protocol.

## Build & run

```sh
cp example.env .env
# edit .env -- fill NEXTENDO_HOST, NEXTENDO_SECURE_PASSWORD, NEXTENDO_SECRET,
# NEXTENDO_INTERNAL_KEY at minimum
go build -o mhgu-server .
./mhgu-server
```

The process needs `cert.pem` and `key.pem` in the working directory (or
wherever `CERT_FILE`/`KEY_FILE` point).

## Architecture

Flat `package main`, mirroring the convention used by `splatoon-2` and
`mario-kart-8-deluxe`:

| File | Purpose |
| --- | --- |
| `main.go` | Entry point. Wires both endpoints, registers all handlers, runs the dashboard + presence reporter, handles SIGINT/SIGTERM for graceful drain. |
| `gates.go` | HTTP integration with `nextendo-account`: online-check (fail-open) and NSA->PID resolution (fail-closed, cached). |
| `presence.go` | 30s batch POST of active PIDs to `nextendo-account` as "playing MHGU". |
| `mhgu_matchmake.go` | MHXX Matchmake Extension (protocol 109, methods 54-59): `UpdateFriendUserProfile`, `GetFriendUserProfiles`, `GetFriends`, `AddFriends`, `RemoveFriend`, `FindCommunityByOwner`. |
| `mhgu_utility.go` | Wraps the core `UtilityHandler` and adds MHGU-specific method stubs. |
| `datastore.go` | DataStore (protocol 0x73) stub. **Not a real implementation** -- Splatoon 2's stub leaves matchmaking incomplete, and MHGU depends on DataStore more. |
| `dashboard.go` | `:8085` JSON monitoring dashboard. Mirrors `splatoon-2`'s layout but with MHGU labels and `max=4` (hunter lobby size). |

## Differences from `splatoon-2`

Improvements over the reference server:

- `slog` (structured JSON) instead of `fmt.Printf`.
- `signal.Notify` + `http.Server.Shutdown` for graceful drain -- important
  for MHGU's hour-long hunts where a server restart would disconnect
  everyone mid-quest.
- `NEXTENDO_REQUIRE_SIGNED_TOKEN=1` is the default. There's no legacy
  MHGU emulator sending bare PIDs, so the documented impersonation
  vulnerability in `splatoon-2` (`main.go:188-193`) doesn't apply.
- Dashboard labels and `max=4` match MHGU's 4-player lobbies.

## Contributing

The first real PRs are going to be:

1. **Find the access key and NEX version.** RE the MHGU binary, fill in
   `accessKey` and `nexVersion` in `main.go`.
2. **Find the ACID.** Wire it into the secure `StationURL` (`g{ACID}-lp1.{...}.n.srv.nintendo.net`-equivalent).
3. **Determine the Pia config + station scheme + PRUDP minor version** by
   capture.
4. **Implement the real DataStore handlers** so Gathering Hall persistence
   works.
5. **Implement the game-specific RMC methods** for quest dispatch, party
   state, Palico data, and lobby chat.

Steps 1-3 unblock the very first console->server handshake. Step 4 makes
lobby listing work. Step 5 makes quests playable.

## License

PolyForm Shield 1.0.0 -- source-available, non-compete. See `LICENSE.md`.

## References

- [kinnay/NintendoClients -- Matchmake Extension Protocol (MHXX)](https://github.com/kinnay/NintendoClients/wiki/Matchmake-Extension-Protocol-(MHXX))
- [kinnay/NintendoClients -- NEX Overview (Game Servers)](https://github.com/kinnay/NintendoClients/wiki/NEX-Overview-(Game-Servers))
- [Pretendo Network](https://pretendo.network) -- Wii U/3DS precedent; their `nex-go` and `nex-protocols-common-go` are the cleanest public reference implementations, even though they don't cover Switch.
- [Nextendo Network](https://nextendo.network)
- [andreluis034/prudp-wireshark-dissector](https://github.com/andreluis034/prudp-wireshark-dissector) -- useful for traffic capture.