package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"miaomiaowu/internal/logger"
	"miaomiaowu/internal/notify"
	"miaomiaowu/internal/storage"
)

func StartNotifyScheduler(ctx context.Context, repo *storage.TrafficRepository, trafficHandler *TrafficSummaryHandler) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	var lastDailyRun string

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			n := GetNotifier()
			if n == nil {
				continue
			}
			cfg := n.GetConfig()

			if cfg.NotifyDailyTraffic {
				today := now.Format("2006-01-02")
				nowTime := now.Format("15:04")
				targetTime := cfg.DailyTrafficTime
				if targetTime == "" {
					targetTime = "08:00"
				}
				if nowTime == targetTime && lastDailyRun != today {
					lastDailyRun = today
					go sendDailyTrafficNotification(ctx, trafficHandler, n)
				}
			}

			if cfg.NotifyExpiry && now.Format("15:04") == "09:00" {
				go sendExpiryNotification(ctx, repo, n)
			}
		}
	}
}

func sendDailyTrafficNotification(ctx context.Context, th *TrafficSummaryHandler, n *notify.Notifier) {
	totalLimitGB, totalUsedGB, probeServers, extSubs, err := th.FetchTrafficSummaryForNotify(ctx)
	if err != nil {
		logger.Warn("[Notify] 获取流量数据失败", "error", err)
		return
	}

	if totalLimitGB == 0 && totalUsedGB == 0 && len(probeServers) == 0 && len(extSubs) == 0 {
		return
	}

	var b strings.Builder

	pct := 0.0
	if totalLimitGB > 0 {
		pct = totalUsedGB / totalLimitGB * 100
	}
	remainGB := totalLimitGB - totalUsedGB
	if remainGB < 0 {
		remainGB = 0
	}
	fmt.Fprintf(&b, "总计: %.2f / %.2f GB (%.1f%%)\n剩余: %.2f GB", totalUsedGB, totalLimitGB, pct, remainGB)

	if len(probeServers) > 0 {
		b.WriteString("\n\n— 服务器 —")
		for _, s := range probeServers {
			fmt.Fprintf(&b, "\n• %s: %.2f / %.2f GB", s.Name, s.UsedGB, s.LimitGB)
		}
	}

	if len(extSubs) > 0 {
		b.WriteString("\n\n— 外部订阅 —")
		for _, s := range extSubs {
			fmt.Fprintf(&b, "\n• %s: %.2f / %.2f GB", s.Name, s.UsedGB, s.LimitGB)
		}
	}

	_ = n.Send(ctx, notify.Event{
		Type:    notify.EventDailyTraffic,
		Title:   "每日流量统计",
		Message: b.String(),
	})
}

func sendExpiryNotification(ctx context.Context, repo *storage.TrafficRepository, n *notify.Notifier) {
	files, err := repo.ListSubscribeFiles(ctx)
	if err != nil {
		logger.Warn("[Notify] 获取订阅文件失败", "error", err)
		return
	}

	now := time.Now()
	threeDaysLater := now.Add(3 * 24 * time.Hour)

	var lines []string
	for _, f := range files {
		if f.ExpireAt == nil {
			continue
		}
		if f.ExpireAt.After(now) && f.ExpireAt.Before(threeDaysLater) {
			days := int(f.ExpireAt.Sub(now).Hours() / 24)
			lines = append(lines, fmt.Sprintf("• %s: %d 天后到期", f.Name, days))
		}
	}

	if len(lines) == 0 {
		return
	}

	msg := strings.Join(lines, "\n")
	_ = n.Send(ctx, notify.Event{
		Type:    notify.EventExpiry,
		Title:   "订阅即将到期",
		Message: msg,
	})
}
