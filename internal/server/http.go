package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/proto"
	"github.com/erickdsama/zerock/internal/version"
)

// readyWait bounds how long a request waits for a tunnel that is mid-handshake.
// Long enough to cover a handshake or reconnect, short enough that a genuinely
// stuck agent does not hold requests open.
const readyWait = 3 * time.Second

// newFrontend builds the handler that serves public traffic: the management API
// on the API host, and tunnelled traffic on every other subdomain.
func (s *Server) newFrontend() http.Handler {
	api := s.newAPIHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)

		switch host {
		case s.cfg.APIHost, s.cfg.Domain:
			api.ServeHTTP(w, r)
			return
		}

		sub, ok := s.cfg.subdomainOf(host)
		if !ok {
			s.writeEdgeError(w, r, http.StatusNotFound, "unknown host",
				fmt.Sprintf("%s is not served by this zerock server.", host))
			return
		}
		tun, claimed := s.reg.claimed(sub)
		if !claimed {
			s.writeEdgeError(w, r, http.StatusNotFound, "no tunnel here",
				fmt.Sprintf("Nothing is currently forwarding %s. Start one with: zerock http <port> --sub %s", host, sub))
			return
		}
		// The tunnel may still be finishing its handshake, which is also the
		// case for a few moments after every reconnect.
		if !tun.waitReady(r.Context(), readyWait) {
			s.writeEdgeError(w, r, http.StatusServiceUnavailable, "tunnel not ready",
				fmt.Sprintf("A tunnel for %s is connecting. Try again in a moment.", host))
			return
		}
		s.proxyHTTP(w, r, tun)
	})
}

// proxyHTTP forwards one request down the tunnel and records what happened.
func (s *Server) proxyHTTP(w http.ResponseWriter, r *http.Request, tun *Tunnel) {
	if !s.checkBasicAuth(w, r, tun) {
		return
	}

	started := time.Now()
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}

	tun.proxy.ServeHTTP(rec, r)

	elapsed := time.Since(started)
	tun.recordRequest(body.n, rec.written)
	tun.emit(proto.Event{
		T:      proto.EventRequest,
		Method: r.Method,
		Path:   requestPath(r),
		Status: rec.status,
		Bytes:  rec.written,
		Millis: float64(elapsed.Microseconds()) / 1000,
		Remote: s.clientIP(r),
	})
}

// buildProxy wires a reverse proxy whose transport dials the agent by opening a
// yamux stream, so the agent never needs an inbound port.
func (s *Server) buildProxy(tun *Tunnel) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tun.openStream()
		},
		// The "connection" is a multiplexed stream, so the usual TCP-oriented
		// pooling knobs matter less; a small idle pool still avoids opening a
		// stream per request under load.
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // long-polling and SSE backends must not be cut off
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true, // pass the client's negotiation through untouched
	}

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// The agent dials a fixed local address, so the authority here is
			// only a placeholder; the original Host is preserved below.
			pr.Out.URL.Host = tun.LocalAddr
			pr.Out.Host = pr.In.Host

			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", s.cfg.scheme())
			pr.Out.Header.Set("X-Zerock-Tunnel", tun.ID)
			if ip := s.clientIP(pr.In); ip != "" {
				pr.Out.Header.Set("X-Forwarded-For", ip)
				pr.Out.Header.Set("X-Real-IP", ip)
			}
			// Credentials consumed at the edge must not leak to the backend.
			if tun.basicAuth != "" {
				pr.Out.Header.Del("Authorization")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Debug("proxy error", "sub", tun.Subdomain, "err", err)
			msg := fmt.Sprintf("The tunnel is up but %s did not answer on %s.", tun.AgentHost, tun.LocalAddr)
			if tun.AgentHost == "" {
				msg = fmt.Sprintf("The tunnel is up but the agent did not answer on %s.", tun.LocalAddr)
			}
			s.writeEdgeError(w, r, http.StatusBadGateway, "local service unreachable", msg)
			tun.emit(proto.Event{
				T:       proto.EventNotice,
				Message: fmt.Sprintf("could not reach %s: %v", tun.LocalAddr, err),
			})
		},
	}
}

// checkBasicAuth enforces edge credentials when the tunnel asked for them.
func (s *Server) checkBasicAuth(w http.ResponseWriter, r *http.Request, tun *Tunnel) bool {
	if tun.basicAuth == "" {
		return true
	}
	wantUser, wantPass, _ := strings.Cut(tun.basicAuth, ":")
	gotUser, gotPass, ok := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(wantPass)) == 1
	if ok && userOK && passOK {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="zerock tunnel", charset="UTF-8"`)
	s.writeEdgeError(w, r, http.StatusUnauthorized, "authentication required",
		"This tunnel is protected. Supply the credentials its owner gave you.")
	return false
}

// clientIP returns the caller's address, trusting forwarding headers only when
// the operator said something trusted sits in front.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
	}
	return hostOnly(r.RemoteAddr)
}

// writeEdgeError renders an edge failure, as JSON for API-ish clients and as a
// small HTML page for browsers.
func (s *Server) writeEdgeError(w http.ResponseWriter, r *http.Request, code int, title, detail string) {
	w.Header().Set("X-Zerock-Server", version.Version)
	if wantsJSON(r) {
		writeJSON(w, code, map[string]string{"error": title, "message": detail})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, edgeErrorPage, code, htmlEscape(title), htmlEscape(title), htmlEscape(detail), version.Version)
}

// redirectToHTTPS is the port 80 handler in TLS modes, leaving the ACME path
// alone so certificate renewal is never redirected away.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		http.NotFound(w, r)
		return
	}
	target := url.URL{Scheme: "https", Host: normalizeHost(r.Host), Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
}

// normalizeHost lowercases a Host header and drops any port.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

// requestPath renders the path with its query for logging.
func requestPath(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + r.URL.RawQuery
}

// recorder captures the status and body size of a response. Unwrap lets
// http.NewResponseController reach the real writer, which is what makes
// WebSocket upgrades and SSE flushing keep working through the wrapper.
type recorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (rec *recorder) WriteHeader(code int) {
	if rec.wroteHeader {
		return
	}
	rec.wroteHeader = true
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.written += int64(n)
	return n, err
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Hijack intercepts protocol upgrades. ReverseProxy serves a 101 by hijacking
// and writing to the raw connection, which would otherwise leave this recorder
// reporting the default 200 and counting none of the upgraded traffic. Taking
// the hijack here keeps WebSocket connections visible in the logs and in the
// tunnel's byte counters.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("zerock: %T does not support hijacking", rec.ResponseWriter)
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	rec.wroteHeader = true
	rec.status = http.StatusSwitchingProtocols
	return &countingConn{Conn: conn, rec: rec}, brw, nil
}

// countingConn folds bytes sent over an upgraded connection into the recorder.
type countingConn struct {
	net.Conn
	rec *recorder
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.rec.written += int64(n)
	return n, err
}

// Flush is kept so handlers that type-assert http.Flusher directly, rather than
// going through a ResponseController, still stream.
func (rec *recorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// countingBody totals how many request-body bytes the backend consumed.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

const edgeErrorPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%d %s</title>
<style>
:root{color-scheme:light dark}
body{margin:0;min-height:100dvh;display:grid;place-items:center;
     font:16px/1.6 ui-sans-serif,system-ui,-apple-system,Segoe UI,sans-serif;
     background:Canvas;color:CanvasText}
main{max-width:32rem;padding:2rem;text-align:center}
h1{margin:0 0 .5rem;font-size:1.25rem;letter-spacing:-.01em}
p{margin:0;opacity:.75}
code{font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;
     background:color-mix(in srgb,CanvasText 8%%,Canvas);padding:.15em .4em;border-radius:.3em}
footer{margin-top:2rem;font-size:.75rem;opacity:.45;letter-spacing:.04em;text-transform:uppercase}
</style>
<main>
  <h1>%s</h1>
  <p>%s</p>
  <footer>zerock %s</footer>
</main>
`
