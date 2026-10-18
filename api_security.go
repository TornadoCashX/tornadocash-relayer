package main

import (
	"container/list"
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	secondsPerMinute = 60
	// rateBucketIdleTTL is how long a bucket may stay unused before it is
	// dropped; after this idle time it has fully refilled for any sane rate.
	rateBucketIdleTTL = 10 * time.Minute
	// minRetryAfter is the smallest Retry-After reported to a limited client.
	minRetryAfter = time.Second
	// ipv6RateLimitPrefixBits groups IPv6 clients per /64 network.
	ipv6RateLimitPrefixBits = 64
	ipv6AddressBits         = 128
	// maxCFRayLength bounds the CF-Ray value copied into access logs.
	maxCFRayLength = 128
)

// rateBucket is one token bucket. key is empty for the shared overflow bucket.
type rateBucket struct {
	key    string
	tokens float64
	last   time.Time
}

// ipRateLimiter is a per-client token bucket limiter with a bounded client
// table.
//
// Buckets live in an LRU list (front = most recently used) indexed by key, so
// lookup, touch and eviction are O(1) under the lock. When the table holds
// maxClients buckets, a new key may evict the least recently used bucket only
// if that bucket carries no limiting state: it has been idle for at least
// rateBucketIdleTTL or has refilled to the full burst. Otherwise the new key is
// charged against one shared overflow bucket (full burst initially). Rotating
// through more keys than maxClients (e.g. the 65536 /64 networks of one IPv6
// /48) therefore never yields fresh full-burst buckets: limited clients keep
// their state and all excess keys share a single budget.
type ipRateLimiter struct {
	mu         sync.Mutex
	perSecond  float64
	burst      float64
	maxClients int
	lru        *list.List // of *rateBucket
	index      map[string]*list.Element
	overflow   *rateBucket
}

func newIPRateLimiter(perMinute, burst, maxClients uint64) *ipRateLimiter {
	return &ipRateLimiter{
		perSecond:  float64(perMinute) / secondsPerMinute,
		burst:      float64(burst),
		maxClients: int(maxClients),
		lru:        list.New(),
		index:      make(map[string]*list.Element),
	}
}

// allow consumes one token for key at now. It reports whether the request is
// admitted and, if not, how long the client should wait (at least
// minRetryAfter). A nil or disabled limiter (rate or burst zero) admits
// everything. Keys without a bucket share the overflow bucket while the table
// is full of buckets that still limit their clients; see ipRateLimiter.
func (l *ipRateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	if l == nil || l.perSecond <= 0 || l.burst <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dropIdle(now)
	return l.take(l.bucketFor(key, now), now)
}

// dropIdle removes buckets unused for rateBucketIdleTTL, oldest first.
func (l *ipRateLimiter) dropIdle(now time.Time) {
	for back := l.lru.Back(); back != nil; back = l.lru.Back() {
		bucket := back.Value.(*rateBucket)
		if now.Sub(bucket.last) < rateBucketIdleTTL {
			return
		}
		delete(l.index, bucket.key)
		l.lru.Remove(back)
	}
}

// bucketFor returns the bucket charged for key, creating or recycling one when
// possible and falling back to the shared overflow bucket otherwise.
func (l *ipRateLimiter) bucketFor(key string, now time.Time) *rateBucket {
	if element, ok := l.index[key]; ok {
		l.lru.MoveToFront(element)
		return element.Value.(*rateBucket)
	}
	if l.lru.Len() < l.maxClients {
		bucket := &rateBucket{key: key, tokens: l.burst, last: now}
		l.index[key] = l.lru.PushFront(bucket)
		return bucket
	}
	if back := l.lru.Back(); back != nil && l.unlimited(back.Value.(*rateBucket), now) {
		bucket := back.Value.(*rateBucket)
		delete(l.index, bucket.key)
		*bucket = rateBucket{key: key, tokens: l.burst, last: now}
		l.index[key] = back
		l.lru.MoveToFront(back)
		return bucket
	}
	if l.overflow == nil {
		l.overflow = &rateBucket{tokens: l.burst, last: now}
	}
	return l.overflow
}

// unlimited reports whether evicting bucket loses no limiting state.
func (l *ipRateLimiter) unlimited(bucket *rateBucket, now time.Time) bool {
	idle := now.Sub(bucket.last)
	return idle >= rateBucketIdleTTL || bucket.tokens+idle.Seconds()*l.perSecond >= l.burst
}

// take refills bucket up to burst for the time elapsed since its last use and
// consumes one token if available.
func (l *ipRateLimiter) take(bucket *rateBucket, now time.Time) (bool, time.Duration) {
	bucket.tokens = min(bucket.tokens+now.Sub(bucket.last).Seconds()*l.perSecond, l.burst)
	bucket.last = now
	if bucket.tokens >= 1 {
		bucket.tokens--
		return true, 0
	}
	wait := time.Duration(((1 - bucket.tokens) / l.perSecond) * float64(time.Second))
	return false, max(wait, minRetryAfter)
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(strings.Trim(remoteAddr, "[]"))
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

const (
	clientIPModeCloudflare = "cloudflare"
	clientIPModeXFF        = "xff"
	clientIPModeNone       = "none"
)

// clientIP resolves the real client address according to CLIENT_IP_MODE:
//
//   - cloudflare: trust CLIENT_IP_HEADER (CF-Connecting-IP) only from a peer in
//     TRUSTED_PROXY_CIDRS. cloudflared overwrites this header, so it cannot be
//     spoofed through the tunnel. Do not use this mode behind another proxy.
//   - xff: walk X-Forwarded-For from the right and return the first address
//     that is not a trusted proxy. Spoof-proof when the proxy appends to XFF.
//   - none: use the socket peer only.
//
// Headers that appear more than once are ignored.
func (r *Relayer) clientIP(req *http.Request) net.IP {
	peer := remoteIP(req.RemoteAddr)
	if !ipInNetworks(peer, r.cfg.TrustedProxyCIDRs) {
		return peer
	}
	switch r.cfg.ClientIPMode {
	case clientIPModeNone:
		return peer
	case clientIPModeXFF:
		values := req.Header.Values("X-Forwarded-For")
		if len(values) != 1 {
			return peer
		}
		hops := strings.Split(values[0], ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(hops[i]))
			if ip == nil {
				return peer
			}
			if !ipInNetworks(ip, r.cfg.TrustedProxyCIDRs) {
				return ip
			}
		}
		return peer
	default:
		values := req.Header.Values(r.cfg.ClientIPHeader)
		if len(values) != 1 {
			return peer
		}
		if forwarded := net.ParseIP(strings.TrimSpace(values[0])); forwarded != nil {
			return forwarded
		}
		return peer
	}
}

func requestSourceDomain(req *http.Request) string {
	for _, header := range []string{"Origin", "Referer"} {
		raw := strings.TrimSpace(req.Header.Get(header))
		if raw == "" || raw == "null" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err == nil && parsed.Hostname() != "" {
			return strings.ToLower(parsed.Hostname())
		}
	}
	return "direct"
}

func cloudflareRay(req *http.Request) string {
	raw := strings.TrimSpace(req.Header.Get("CF-Ray"))
	if raw == "" || len(raw) > maxCFRayLength {
		return "none"
	}
	if strings.ContainsFunc(raw, notCFRayChar) {
		return "invalid"
	}
	return raw
}

// notCFRayChar reports a character outside the CF-Ray alphabet [A-Za-z0-9-].
func notCFRayChar(char rune) bool {
	return !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-')
}

// rateLimitKey buckets IPv4 per address and IPv6 per /64: one IPv6 client
// usually controls a whole /64 and could otherwise rotate addresses freely.
func rateLimitKey(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(ipv6RateLimitPrefixBits, ipv6AddressBits)).String() + "/" + strconv.Itoa(ipv6RateLimitPrefixBits)
}

func (r *Relayer) withRelayRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && (req.URL.Path == "/relay" || req.URL.Path == "/v1/tornadoWithdraw") {
			ip := r.clientIP(req)
			allowed, retryAfter := r.rateLimiter.allow(rateLimitKey(ip), time.Now())
			if !allowed {
				r.rateLimitRejections.Add(1)
				seconds := int64((retryAfter + time.Second - 1) / time.Second)
				w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Relayer) requireHealthAuth(w http.ResponseWriter, req *http.Request) bool {
	if r.authorizeHealth(req) {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
	return false
}

// requireHealthGET admits only authenticated GET requests to the health and
// metrics endpoints: 405 for other methods, then requireHealthAuth.
func (r *Relayer) requireHealthGET(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return r.requireHealthAuth(w, req)
}

func (r *Relayer) authorizeHealth(req *http.Request) bool {
	values := req.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	provided, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || r.cfg.HealthToken == "" {
		return false
	}
	return len(provided) == len(r.cfg.HealthToken) && subtle.ConstantTimeCompare([]byte(provided), []byte(r.cfg.HealthToken)) == 1
}
