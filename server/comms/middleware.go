// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package comms

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"decred.org/dcrdex/dex"
)

type contextKey int

// These are the keys for different types of values stored in a request context.
const (
	CtxThing contextKey = iota
	ctxListener
)

// LimitRate is rate-limiting middleware that checks whether a request can be
// fulfilled. This is intended for the /api HTTP endpoints.
func (s *Server) LimitRate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, err := s.meterIP(clientIP(r))
		if err != nil {
			http.Error(w, err.Error(), code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the rate limiting key for an HTTP request. It is the TCP peer
// address, except when the peer is a reverse proxy on the same host
// (127.0.0.1 or ::1) that set X-Forwarded-For. Then it is the address the
// proxy appended, i.e. the last X-Forwarded-For entry. Earlier entries come
// from the client and can be forged, so they are never used. Headers from
// any other peer are ignored, so remote clients cannot pick their own key.
func clientIP(r *http.Request) dex.IPKey {
	remote := dex.NewIPKey(r.RemoteAddr)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.Trim(r.RemoteAddr, "[]")
	}
	peer := net.ParseIP(host)
	if peer == nil || !(peer.Equal(net.IPv4(127, 0, 0, 1)) || peer.Equal(net.IPv6loopback)) {
		return remote
	}
	vals := r.Header.Values("X-Forwarded-For")
	if len(vals) == 0 {
		return remote
	}
	entries := strings.Split(vals[len(vals)-1], ",")
	fwd := strings.TrimSpace(entries[len(entries)-1])
	if net.ParseIP(strings.Trim(fwd, "[]")) == nil {
		return remote
	}
	return dex.NewIPKey(fwd)
}

// meterIP applies the dataEnabled flag, the per-IP rate limiter, and the
// global HTTP rate limiter. This is only intended for the data API. The other
// websocket route handlers have different limiters that are shared between
// connections from the same IP, but not globally.
//
// The per-IP limiter is checked before the global limiter so that a single
// abusive source is rejected without consuming global rate limit tokens.
func (s *Server) meterIP(ip dex.IPKey) (int, error) {
	if atomic.LoadUint32(&s.dataEnabled) != 1 {
		return http.StatusServiceUnavailable, fmt.Errorf("data API is disabled")
	}
	ipLimiter := s.getIPLimiter(ip)
	if !ipLimiter.Allow() {
		return http.StatusTooManyRequests, fmt.Errorf("too many requests")
	}
	if !s.globalHTTPRateLimiter.Allow() {
		return http.StatusTooManyRequests, fmt.Errorf("too many global requests")
	}
	return 0, nil
}
