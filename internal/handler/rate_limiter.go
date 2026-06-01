package handler

import (
	"errors"
	"sync"
	"time"

	"miaomiaowu/internal/logger"
)

// 使用英文错误消息, 防止老外看不懂
var ErrRateLimited = errors.New("rate limit exceeded")

var globalLoginRateLimiter *LoginRateLimiter

func GetLoginRateLimiter() *LoginRateLimiter {
	return globalLoginRateLimiter
}

type attemptInfo struct {
	count     int
	firstTime time.Time
	lockUntil time.Time
}

type LoginRateLimiter struct {
	mu              sync.RWMutex
	ipAttempts      sync.Map // IP -> *attemptInfo
	accountAttempts sync.Map // username -> *attemptInfo
	maxAttempts     int
	windowDuration  time.Duration
	lockDuration    time.Duration
	skipLocalIP     bool
}

func NewLoginRateLimiter() *LoginRateLimiter {
	l := &LoginRateLimiter{
		maxAttempts:    5,
		windowDuration: time.Hour,
		lockDuration:   time.Hour,
		skipLocalIP:    true,
	}
	globalLoginRateLimiter = l
	return l
}

func NewLoginRateLimiterWithConfig(maxAttempts, windowMinutes, lockMinutes int) *LoginRateLimiter {
	l := &LoginRateLimiter{
		maxAttempts:    maxAttempts,
		windowDuration: time.Duration(windowMinutes) * time.Minute,
		lockDuration:   time.Duration(lockMinutes) * time.Minute,
		skipLocalIP:    true,
	}
	globalLoginRateLimiter = l
	return l
}

func (l *LoginRateLimiter) UpdateConfig(maxAttempts, windowMinutes, lockMinutes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxAttempts = maxAttempts
	l.windowDuration = time.Duration(windowMinutes) * time.Minute
	l.lockDuration = time.Duration(lockMinutes) * time.Minute
}

func (l *LoginRateLimiter) getConfig() (int, time.Duration, time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.maxAttempts, l.windowDuration, l.lockDuration
}

func (l *LoginRateLimiter) SetSkipLocalIP(skip bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.skipLocalIP = skip
}

func (l *LoginRateLimiter) shouldSkipIP(ip string) bool {
	l.mu.RLock()
	skip := l.skipLocalIP
	l.mu.RUnlock()
	return skip && IsLocalOrPrivateIP(ip)
}

func (l *LoginRateLimiter) Check(ip, username string) error {
	now := time.Now()

	if !l.shouldSkipIP(ip) {
		if err := l.checkAttempts(&l.ipAttempts, ip, now); err != nil {
			logger.Warn("🚫🚫🚫 [RATE_LIMIT] 登录被限制（IP）",
				"ip", ip,
				"username", username,
			)
			return err
		}
	}

	if username != "" {
		if err := l.checkAttempts(&l.accountAttempts, username, now); err != nil {
			logger.Warn("🚫🚫🚫 [RATE_LIMIT] 登录被限制（账户）",
				"ip", ip,
				"username", username,
			)
			return err
		}
	}

	return nil
}

func (l *LoginRateLimiter) checkAttempts(store *sync.Map, key string, now time.Time) error {
	maxAttempts, windowDuration, lockDuration := l.getConfig()

	val, _ := store.Load(key)
	if val == nil {
		return nil
	}

	info := val.(*attemptInfo)

	if !info.lockUntil.IsZero() && now.Before(info.lockUntil) {
		return ErrRateLimited
	}

	if !info.lockUntil.IsZero() && now.After(info.lockUntil) {
		store.Delete(key)
		return nil
	}

	if now.Sub(info.firstTime) > windowDuration {
		store.Delete(key)
		return nil
	}

	if info.count >= maxAttempts {
		info.lockUntil = now.Add(lockDuration)
		return ErrRateLimited
	}

	return nil
}

func (l *LoginRateLimiter) RecordFailure(ip, username string) {
	now := time.Now()

	if !l.shouldSkipIP(ip) {
		l.recordAttempt(&l.ipAttempts, ip, now)
	}
	if username != "" {
		l.recordAttempt(&l.accountAttempts, username, now)
	}
}

func (l *LoginRateLimiter) recordAttempt(store *sync.Map, key string, now time.Time) {
	_, windowDuration, _ := l.getConfig()

	val, loaded := store.Load(key)
	if !loaded {
		store.Store(key, &attemptInfo{
			count:     1,
			firstTime: now,
		})
		return
	}

	info := val.(*attemptInfo)

	if now.Sub(info.firstTime) > windowDuration {
		store.Store(key, &attemptInfo{
			count:     1,
			firstTime: now,
		})
		return
	}

	info.count++
}

func (l *LoginRateLimiter) RecordSuccess(ip, username string) {
	l.ipAttempts.Delete(ip)
	if username != "" {
		l.accountAttempts.Delete(username)
	}
}
