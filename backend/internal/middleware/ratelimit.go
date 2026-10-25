package middleware

import (
	"errors"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/ratelimit"
	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/pkg/response"
)

// RateLimitByUser allows a signed-in user a limited number of requests. It
// MUST run after Auth: it keys on the user ID Auth stored. Over the limit it
// answers 429 with a Retry-After header, and the request takes no token.
func RateLimitByUser(l *ratelimit.KeyedLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := UserIDFrom(c)
		if !ok {
			response.InternalError(c, errors.New("rate limit: user id missing from context (is Auth registered before it?)"))
			return
		}
		if allowed, wait := l.Allow(userID.String()); !allowed {
			rejectRate(c, wait)
			return
		}
		c.Next()
	}
}

// RateLimitByIP limits by client address, for routes that run BEFORE anyone is
// signed in (login, register). c.ClientIP() is the real peer address only
// because the router trusts no proxy; behind a reverse proxy, configure the
// trusted proxies first or every user will share one bucket.
func RateLimitByIP(l *ratelimit.KeyedLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if allowed, wait := l.Allow(clientKey(c.ClientIP())); !allowed {
			rejectRate(c, wait)
			return
		}
		c.Next()
	}
}

// LimitConcurrencyByUser lets a user have only a few requests RUNNING at once.
// It matters for streaming answers, which last a long time. The slot is
// released by a defer, so it is freed after a normal finish, after a panic
// (the recovery middleware sits further out), and when the client disconnects
// (the handler then returns). Like RateLimitByUser it must run after Auth.
func LimitConcurrencyByUser(s *ratelimit.KeyedSemaphore) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := UserIDFrom(c)
		if !ok {
			response.InternalError(c, errors.New("concurrency limit: user id missing from context (is Auth registered before it?)"))
			return
		}
		release, ok := s.TryAcquire(userID.String())
		if !ok {
			rejectConcurrency(c)
			return
		}
		defer release()
		c.Next()
	}
}

func rejectRate(c *gin.Context, wait time.Duration) {
	secs := retryAfterSeconds(wait)
	c.Header("Retry-After", strconv.Itoa(secs))
	response.ErrorWithDetails(c, http.StatusTooManyRequests, response.CodeRateLimited,
		"you are sending requests too fast, please wait a moment",
		gin.H{"reason": "rate_limit", "retry_after_seconds": secs})
}

// rejectConcurrency has NO Retry-After on purpose: an answer may finish in
// seconds or in minutes, and a made-up number would mislead clients.
func rejectConcurrency(c *gin.Context) {
	response.ErrorWithDetails(c, http.StatusTooManyRequests, response.CodeRateLimited,
		"you already have the maximum number of questions or summaries running, wait for one to finish",
		gin.H{"reason": "concurrent_limit"})
}

// retryAfterSeconds rounds UP, and is at least 1: waiting the advertised time
// must be enough.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

// clientKey turns a client address into the key to limit by. IPv6 clients
// usually control a whole /64, so those are limited as a group; an IPv4-mapped
// IPv6 address counts as the IPv4 address. Anything unreadable shares one key.
func clientKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "unknown"
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}