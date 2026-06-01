package handler

import (
	"context"
	"sync"
	"time"

	"miaomiaowu/internal/logger"
)

var globalSubscriptionRateLimiter *SubscriptionRateLimiter

func GetSubscriptionRateLimiter() *SubscriptionRateLimiter {
	return globalSubscriptionRateLimiter
}

type subRateRecord struct {
	count       int
	windowStart time.Time
}

type SubscriptionRateLimiter struct {
	mu      sync.Mutex
	ips     map[string]*subRateRecord
	enabled     bool
	limit       int
	window      time.Duration
	skipLocalIP bool
}

func NewSubscriptionRateLimiter(limit int, window time.Duration) *SubscriptionRateLimiter {
	if limit <= 0 {
		limit = 60
	}
	if window <= 0 {
		window = time.Minute
	}
	l := &SubscriptionRateLimiter{
		ips:         make(map[string]*subRateRecord),
		enabled:     true,
		limit:       limit,
		window:      window,
		skipLocalIP: true,
	}
	globalSubscriptionRateLimiter = l
	return l
}

func (l *SubscriptionRateLimiter) SetSkipLocalIP(skip bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.skipLocalIP = skip
}

func (l *SubscriptionRateLimiter) UpdateConfig(enabled bool, limit, windowMinutes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = enabled
	if limit > 0 {
		l.limit = limit
	}
	if windowMinutes > 0 {
		l.window = time.Duration(windowMinutes) * time.Minute
	}
}

// Allow 返回该 IP 此刻是否允许再发起一次订阅获取。
func (l *SubscriptionRateLimiter) Allow(ip string) bool {
	if ip == "" {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.enabled {
		return true
	}
	if l.skipLocalIP && IsLocalOrPrivateIP(ip) {
		return true
	}

	rec, ok := l.ips[ip]
	if !ok || now.Sub(rec.windowStart) > l.window {
		l.ips[ip] = &subRateRecord{count: 1, windowStart: now}
		return true
	}
	rec.count++
	if rec.count > l.limit {
		if rec.count == l.limit+1 {
			logger.Warn("🚦 [SUB_RATE] 订阅获取频率超限,已限流", "ip", ip, "limit", l.limit, "window", l.window.String())
		}
		return false
	}
	return true
}

// StartCleanup 定期清理过期 IP 记录,避免 map 无限增长。
func (l *SubscriptionRateLimiter) StartCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			l.mu.Lock()
			for ip, rec := range l.ips {
				if now.Sub(rec.windowStart) > l.window {
					delete(l.ips, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}
