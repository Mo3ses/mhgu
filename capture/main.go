// Quick BAAS auth capture server.
// Listens on :8443 with the project's self-signed cert (which has SAN for
// g2896bd04-lp1.s.n.srv.nintendo.net, so MHGU will accept it after trust).
// Logs every request, then returns a stub JSON that may be enough for MHGU
// to proceed past the BAAS step.
//
// To use: stop mhgu-server, then either (a) run this on :443 directly
// (replaces mhgu-server during capture), or (b) run on :8443 and add
// `iptables -t nat -A OUTPUT -d 51.178.29.194 -p tcp --dport 443 -j DNAT
// --to-destination 127.0.0.1:8443` so MHGU's BAAS attempt lands here.
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := ":8443"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	certFile := os.Getenv("CERT_FILE")
	if certFile == "" {
		certFile = "cert.pem"
	}
	keyFile := os.Getenv("KEY_FILE")
	if keyFile == "" {
		keyFile = "key.pem"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("[%s] %s %s%s host=%q ua=%q body=%dB",
			time.Now().Format(time.RFC3339), r.Method, r.Host, r.URL.Path,
			r.Host, r.UserAgent(), len(body))
		// Dump headers
		for k, v := range r.Header {
			log.Printf("  H %s: %s", k, v)
		}
		if len(body) > 0 && len(body) < 4096 {
			log.Printf("  BODY: %s", string(body))
		} else if len(body) >= 4096 {
			log.Printf("  BODY[%dB]: %s...", len(body), string(body[:512]))
		}
		// Stub: minimal JSON MHGU might accept
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// We don't know the exact format yet — print 200 OK with body
		// so MHGU at least gets past the SSL auth step. Replace with the
		// real JSON once we know what it expects.
		fmt.Fprint(w, `{"status":"ok","stub":true}`)
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("BAAS capture server listening on %s (TLS, cert=%s key=%s)", addr, certFile, keyFile)
	log.Fatal(srv.ListenAndServeTLS(certFile, keyFile))
}
