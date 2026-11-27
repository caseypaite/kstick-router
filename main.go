// router-webui: web administration for the KStick 4G LTE router.
package main

import (
	"crypto/tls"
	"embed"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

// Only a small "UI files missing" page is built in: the real UI is served from -web-dir (or -web-dir-xp, ui.go), so a UI change
// does not change the 12 MB binary and a UI-only deploy uploads just the changed files.
//
//go:embed webfallback
var webFS embed.FS

var (
	flagConfig  = flag.String("config", "/etc/router/router.json", "router configuration file")
	flagHTTPS   = flag.String("https", ":443", "HTTPS listen address")
	flagHTTP    = flag.String("http", ":80", "HTTP listen address (redirects to HTTPS); empty disables")
	flagTLSDir  = flag.String("tls-dir", "/etc/router/tls", "directory holding the TLS certificate")
	flagUser    = flag.String("admin-user", "user", "system account whose password grants admin access")
	flagRender  = flag.Bool("render", false, "write firewall, dnsmasq and WAN files from the config, then exit")
	flagSetMode = flag.String("set-mode", "", "switch to \"router\" or \"client\" (Wi-Fi client) mode without the confirm step, restart the running web UI, then exit")
	flagWebDir  = flag.String("web-dir", "/usr/local/share/router-webui/web", "directory with the classic UI files (index.html, app.js, style.css); for development use -web-dir ./web (the XP UI is then read from ./web-xp)")
)

func main() {
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("LC_ALL", "C.UTF-8") // English tool output, but keep UTF-8 (SMS text)

	if err := loadConfig(*flagConfig); err != nil {
		log.Fatalf("config: %v", err)
	}
	loadModules()
	if *flagRender {
		renderAll()
		return
	}
	if *flagSetMode != "" {
		if err := cliSetMode(*flagSetMode); err != nil {
			log.Fatalf("set-mode: %v", err)
		}
		return
	}
	cert, err := ensureCert(*flagTLSDir)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}

	go sampler()
	go modemLoop()
	go netWatch()
	go vpnLoop()
	go ipv6Loop()
	go smsStoreLoop()
	go updateBlockLoop()
	reapplyIfLANIfChanged()
	revertPendingAtStart() // a risky change that was never confirmed
	go usageLoop()
	go historyLoop()
	go batteryLoop()
	go signalLoop()
	go scheduleLoop()
	go btSetup()
	go btUplinkLoop()
	go thermalGuardLoop()
	go lcdLoop()

	mux := http.NewServeMux()
	registerAPI(mux)
	mux.Handle("GET /", webHandler())

	srv := &http.Server{
		Addr:              *flagHTTPS,
		Handler:           guard(mux),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      180 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0), // self-signed cert => noisy handshake errors
	}
	if *flagHTTP != "" {
		go func() {
			redir := &http.Server{
				Addr: *flagHTTP,
				Handler: guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					host := r.Host
					if h, _, err := net.SplitHostPort(host); err == nil {
						host = h
					}
					_, port, _ := net.SplitHostPort(*flagHTTPS)
					if port != "" && port != "443" {
						host = net.JoinHostPort(host, port)
					}
					http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
				})),
				ReadHeaderTimeout: 10 * time.Second,
			}
			log.Printf("http redirect: %v", redir.ListenAndServe())
		}()
	}
	log.Printf("router web UI listening on %s (admin account %q)", *flagHTTPS, *flagUser)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// guard only serves clients on local networks (LAN, USB link, loopback) — a second
// line of defence behind the firewall — and sets strict security headers.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || isMeshIP(ip)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; style-src-attr 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		if r.TLS != nil {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.ServeHTTP(w, r)
	})
}

// Cloudflare Mesh / Zero Trust device virtual IPs (reach us only if the firewall allows).
var meshNets = []*net.IPNet{mustCIDR("100.96.0.0/12"), mustCIDR("2606:4700:cf1:1000::/64")}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func isMeshIP(ip net.IP) bool {
	for _, n := range meshNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
