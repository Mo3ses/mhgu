// Package main is the MHGU (Monster Hunter Generations Ultimate) NEX game
// server for the Nextendo Network. It registers the standard Nextendo
// protocol handlers on top of nextendo-nex plus the MHXX-specific Matchmake
// Extension Protocol methods (ID 109, methods 54-59).
//
// See README.md for the list of reverse-engineering work still required
// before the server can actually serve a real Switch console.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	nex "github.com/NextendoNetwork/nextendo-nex"
)

// --- Constants ------------------------------------------------------------
//
// TODO: All three of the placeholders below must be filled in by reading
// the MHGU binary. See README.md -> "What's missing".

const (
	// mhguAppID is the Nintendo Switch title ID for Monster Hunter
	// Generations Ultimate. Verified against nextendo-account/games.go:362.
	mhguAppID = "0100770008dd8000"

	// accessKey is the per-title 8-hex string NEX uses to sign the
	// PRUDP handshake. Splatoon 2 uses "4eb18d39", MK8D "09c1c475",
	// SSBU "9587602b". MHGU's value is unknown until RE.
	accessKey = "TODO_MHGU_ACCESS_KEY"

	// nexVersion is the NEX protocol version (e.g. 40000 = NEX 4.0.0).
	// MK8D uses 40000; MHGU's value is unknown until RE.
	nexVersion = 0 // TODO

	// securePID is the "account" PID whose Kerberos key derives the
	// ticket the console uses to authenticate against the secure endpoint.
	// Every Nextendo game server uses 2.
	securePID uint64 = 2

	// sessionKeyLen is the Kerberos session key length, in bytes.
	sessionKeyLen = 32

	// ServerName is the human-readable name returned on LoginEx.
	ServerName = "Nextendo MHGU"
)

// Env var names (kept as constants so example.env and the loader agree).
const (
	envHost             = "NEXTENDO_HOST"
	envAuthPort         = "AUTH_PORT"
	envSecurePort       = "SECURE_PORT"
	envCertFile         = "CERT_FILE"
	envKeyFile          = "KEY_FILE"
	envSecurePassword   = "NEXTENDO_SECURE_PASSWORD"
	envAccountURL       = "NEXTENDO_ACCOUNT_URL"
	envInternalKey      = "NEXTENDO_INTERNAL_KEY"
	envRequireAccount   = "NEXTENDO_REQUIRE_ACCOUNT"
	envRequireSignedTok = "NEXTENDO_REQUIRE_SIGNED_TOKEN"
	envPresenceInterval = "NEXTENDO_PRESENCE_INTERVAL"
	envSecret           = "NEXTENDO_SECRET"
	envSecretFile       = "NEXTENDO_SECRET_FILE"
	envProxyProto       = "NEXTENDO_PROXY_PROTOCOL"
	envDashPort         = "DASH_PORT"
	envDashToken        = "DASH_TOKEN"
	envLogLevel         = "LOG_LEVEL"
)

var (
	logger       *slog.Logger
	nextendoHost string
	authPort     int
	securePort   int
	certFile     string
	keyFile      string
	securePwd    string
	dashPort     int
	dashToken    string
)

// envOr returns the env var named key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envOrInt returns the env var named key parsed as int, or def on
// parse failure / unset.
func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envOrBool returns the env var named key parsed as "1"/"true".
func envOrBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// loadNextendoSecret returns the shared HMAC secret used to sign nex-PID
// tokens, byte-for-byte compatible with what nextendo-account writes.
// Order: raw env var -> hex-decoded file -> nil. The hex-file path must
// match the deployed account server, or HMAC verification will silently
// never match.
func loadNextendoSecret() []byte {
	if v := os.Getenv(envSecret); v != "" {
		return []byte(v)
	}
	path := envOr(envSecretFile, "nextendo_secret.key")
	if b, err := os.ReadFile(path); err == nil {
		if dec, derr := hex.DecodeString(strings.TrimSpace(string(b))); derr == nil && len(dec) >= 16 {
			return dec
		}
	}
	return nil
}

// loadConfig reads every env var once, into package-level vars. Called
// from main() before anything that depends on the configuration.
func loadConfig() error {
	nextendoHost = envOr(envHost, "nextendo.example")
	authPort = envOrInt(envAuthPort, 443)
	securePort = envOrInt(envSecurePort, 60005)
	certFile = envOr(envCertFile, "cert.pem")
	keyFile = envOr(envKeyFile, "key.pem")
	securePwd = envOr(envSecurePassword, "securepasswordplz1")
	dashPort = envOrInt(envDashPort, 8085)
	dashToken = os.Getenv(envDashToken)

	if _, err := os.Stat(certFile); err != nil {
		return fmt.Errorf("cert file %s: %w", certFile, err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		return fmt.Errorf("key file %s: %w", keyFile, err)
	}
	return nil
}

// setupLogger configures the package-level slog logger. Defaults to
// info-level text output on stderr; set LOG_LEVEL=debug for verbose.
func setupLogger() {
	lvl := slog.LevelInfo
	switch strings.ToLower(envOr(envLogLevel, "info")) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	logger = slog.New(h).With(slog.String("svc", "mhgu"))
}

// --- resolveUser (the LoginEx hook) --------------------------------------

// resolveUser is the ResolveUser callback injected into nex.AuthConfig.
// It runs once per LoginEx. Three paths, in order:
//
//  1. Signed token (the only authenticated path). Validates
//     `nx2.<b64(pid.username.expiry)>.<b64(hmac)>` against nextendoSecret.
//  2. Bare numeric PID. Allowed only when NEXTENDO_REQUIRE_SIGNED_TOKEN
//     is false (off by default; on by default in MHGU -- we have no
//     legacy emulator sending bare PIDs).
//  3. Anonymous (hashed username -> 1800000000+). Only when
//     NEXTENDO_REQUIRE_ACCOUNT is false.
//
// Returns pid, sourceKey, ok. sourceKey is the 16-byte HMAC key the
// console uses to decrypt the ClientTicket. For paths 1 and 2 it is
// derived from nextendoSecret so the ticket round-trips.
func resolveUser(username string, extraData []byte) (uint64, []byte, bool) {
	secret := loadNextendoSecret()

	// Path 1: signed token.
	if strings.HasPrefix(username, "nx2.") {
		pid, ok := nextendoPIDFromToken(username, secret)
		if ok {
			return pid, secret, true
		}
		return 0, nil, false
	}

	// Path 2: bare numeric PID. Disabled by default.
	if !envOrBool(envRequireSignedTok, true) {
		if n, err := strconv.ParseUint(username, 10, 64); err == nil {
			logger.Warn("accepting bare PID", slog.Uint64("pid", n))
			return n, secret, true
		}
	}

	// Path 3: anonymous (only when account requirement is off).
	if !envOrBool(envRequireAccount, true) {
		return anonymousPID(username), secret, true
	}

	logger.Debug("login rejected", slog.String("username", username))
	return 0, nil, false
}

// nextendoPIDFromToken validates a nex-PID token and returns the PID.
//
// Token format: nx2.<b64(pid.username.expiry)>.<b64(hmac-sha256)>
// where hmac = HMAC-SHA-256(secret, "nex:"+b64_inner).
func nextendoPIDFromToken(token string, secret []byte) (uint64, bool) {
	if len(secret) == 0 {
		return 0, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "nx2" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	want, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("nex:" + parts[1]))
	if !hmac.Equal(mac.Sum(nil), want) {
		return 0, false
	}

	// Format: <pid>.<username>.<expiry>
	inner := strings.Split(string(raw), ".")
	if len(inner) < 3 {
		return 0, false
	}
	pid, err := strconv.ParseUint(inner[0], 10, 64)
	if err != nil {
		return 0, false
	}
	// Expiry check (best-effort; ignore malformed).
	if exp, err := strconv.ParseInt(inner[2], 10, 64); err == nil && exp < time.Now().Unix() {
		return 0, false
	}
	return pid, true
}

// anonymousPID maps a free-text username to a stable PID in the
// 1800000000 + (FNV-1a hash mod 100000000) range.
func anonymousPID(name string) uint64 {
	const (
		offset = uint64(1800000000)
		span   = uint64(100000000)
	)
	var h uint64 = 1469598103934665603
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return offset + (h % span)
}

// --- main -----------------------------------------------------------------

func main() {
	setupLogger()
	if err := loadConfig(); err != nil {
		logger.Error("config error", slog.Any("err", err))
		os.Exit(2)
	}
	loadGatesConfig() // uses logger.Warn; must run after setupLogger()

	settings := nex.NewSwitchSettings(accessKey, nexVersion)
	if accessKey == "TODO_MHGU_ACCESS_KEY" || nexVersion == 0 {
		logger.Warn("accessKey or nexVersion is unset -- server will boot but console handshake will fail",
			slog.String("accessKey", accessKey),
			slog.Int("nexVersion", nexVersion))
	}

	secureURL := nex.NewStationURL("prudps")
	secureURL.Set("address", nextendoHost)
	secureURL.SetInt("port", securePort)
	secureURL.SetInt("CID", 1)
	secureURL.SetInt("PID", int(securePID))
	secureURL.SetInt("sid", 1)
	secureURL.SetInt("stream", 10)
	secureURL.SetInt("type", 2) // public

	// --- Auth endpoint (TicketGranting, insecure, :443) ---------------
	authEndpoint := nex.NewEndpoint(settings)
	authCfg := &nex.AuthConfig{
		Settings:         settings,
		SecurePID:        securePID,
		SecurePassword:   securePwd,
		SecureStationURL: secureURL,
		ServerName:       ServerName,
		SessionKeyLength: sessionKeyLen,
		ResolveUser:      resolveUser,
	}
	authEndpoint.Register(nex.ProtocolTicketGranting, authCfg.Handler())
	authEndpoint.OnRMC = logRMC("auth")
	authServer := nex.NewServer(authEndpoint)

	// --- Secure endpoint (game, :60005) --------------------------------
	// Splatoon 2 uses a SEPARATE Settings instance for the secure side so
	// it can pin PrudpMinorVersion=0 without breaking SSBU auth. Same
	// trick here until we have evidence otherwise.
	secureSettings := nex.NewSwitchSettings(accessKey, nexVersion)
	secureSettings.PrudpMinorVersion = 0
	secureEndpoint := nex.NewEndpoint(secureSettings)
	secureEndpoint.SetSecureAccount(securePwd, securePID)

	mm := nex.NewMatchmaking()
	mm.FindByParticipantEnabled = true // MHGU Gathering Halls accept friend joins

	// TODO: empirically determine whether MHGU needs LegacyPiaConfig or
	// SwitchPia519Config. S2 uses LegacyPia; SSBU uses 519. MHGU is in
	// between. Replace this with the right one once known.
	secureEndpoint.Register(nex.ProtocolSecureConnection,
		nex.SecureConnectionHandlerWithConfig(nex.LegacyPiaConfig()))
	secureEndpoint.Register(nex.ProtocolMatchMaking, mm.MatchMakingHandler())
	secureEndpoint.Register(nex.ProtocolMatchMakingExt, mm.MatchMakingExtHandler())
	secureEndpoint.Register(nex.ProtocolNATTraversal, nex.NATTraversalHandler())
	secureEndpoint.Register(nex.ProtocolRanking, nex.RankingHandler())

	// The handlers below override the defaults via the wrapper pattern
	// (last-wins on the endpoint's protocol-id map). See each file's
	// doc comment for the rationale.
	setupMHGUMatchmake(secureEndpoint, mm) // 0x6D MHXX (54-59)
	setupMHGUUtility(secureEndpoint)       // 0x6E wrapper, delegates to base
	setupMHGUDataStore(secureEndpoint)     // 0x73 stub

	// --- Hooks ---------------------------------------------------------
	secureEndpoint.OnRMC = func(c *nex.Connection, req *nex.RMCMessage) {
		logRMC("secure")(c, req)
		noteRMC(c, req)
		notePresenceSeen(c.PID)
	}
	secureEndpoint.OnConnect = func(c *nex.Connection) {
		logger.Info("connect",
			slog.String("addr", c.RemoteAddr),
			slog.Uint64("pid", c.PID),
			slog.Uint64("cid", uint64(c.ID)))
	}
	secureEndpoint.OnDisconnect = func(c *nex.Connection) {
		logger.Info("disconnect",
			slog.Uint64("pid", c.PID),
			slog.Uint64("cid", uint64(c.ID)))
		mm.RemovePlayer(c.PID)
	}

	secureEndpoint.StartReaper()
	secureServer := nex.NewServer(secureEndpoint)

	// --- Background services ------------------------------------------
	dashSrv := startDashboard(secureEndpoint, mm)
	stopPresence := startPresenceReporter(secureEndpoint)

	// --- Listeners with graceful shutdown -----------------------------
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	proxyProto := envOrBool(envProxyProto, false)
	authErrCh := make(chan error, 1)
	secureErrCh := make(chan error, 1)

	go func() {
		logger.Info("auth listener starting",
			slog.String("addr", fmt.Sprintf(":%d", authPort)),
			slog.Bool("proxyProtocol", proxyProto))
		var err error
		if proxyProto {
			err = authServer.ListenSecureProxy(authPort, certFile, keyFile)
		} else {
			err = authServer.ListenSecure(authPort, certFile, keyFile)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			authErrCh <- err
		}
	}()
	go func() {
		logger.Info("secure listener starting",
			slog.String("addr", fmt.Sprintf(":%d", securePort)))
		if err := secureServer.ListenSecure(securePort, certFile, keyFile); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			secureErrCh <- err
		}
	}()

	logger.Info("MHGU server ready",
		slog.String("host", nextendoHost),
		slog.Int("authPort", authPort),
		slog.Int("securePort", securePort))

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining...")
	case err := <-authErrCh:
		logger.Error("auth listener died", slog.Any("err", err))
	case err := <-secureErrCh:
		logger.Error("secure listener died", slog.Any("err", err))
	}

	// Graceful drain. Stop accepting new connections, kick the reaper,
	// close existing ones. The reaper keeps running on its own goroutine
	// but StartReaper has no Stop; it ticks every NEXTENDO_REAP_EVERY_SECONDS.
	stopPresence()
	if dashSrv != nil {
		shutdownCtx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer c2()
		_ = dashSrv.Shutdown(shutdownCtx)
	}
	logger.Info("bye")
}

// logRMC returns an OnRMC hook that logs every RMC method call. Use it
// during bring-up so the missing-method flood is visible.
func logRMC(tag string) func(*nex.Connection, *nex.RMCMessage) {
	return func(c *nex.Connection, req *nex.RMCMessage) {
		logger.Debug("rmc",
			slog.String("tag", tag),
			slog.Uint64("pid", c.PID),
			slog.String("proto", fmt.Sprintf("0x%x", req.Protocol)),
			slog.Uint64("method", uint64(req.Method)),
			slog.Uint64("callID", uint64(req.CallID)),
			slog.Int("bodyLen", len(req.Body)))
	}
}

// Compile-time guarantee: envOr returns empty for unset keys. (Avoids the
// "declared and not used" trap when scaffolding.)
var _ = envOr("", "")