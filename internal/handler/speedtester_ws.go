package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"miaomiaowu/internal/speedtest"
	"miaomiaowu/internal/storage"
)

type stWSMsg struct {
	Type        string  `json:"type"`
	JobID       string  `json:"job_id,omitempty"`
	ClashConfig string  `json:"clash_config,omitempty"`
	Bytes       int64   `json:"bytes,omitempty"`
	URL         string  `json:"url,omitempty"`
	Threads     int     `json:"threads,omitempty"`
	LatencyOnly bool    `json:"latency_only,omitempty"` // true 仅测真连接延迟(Cloudflare 204)
	DownMbps    float64 `json:"down_mbps,omitempty"`
	LatencyMs   int64   `json:"latency_ms,omitempty"`
	EgressIP    string  `json:"egress_ip,omitempty"`
	Status      string  `json:"status,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type testerConn struct {
	id      int64
	conn    *websocket.Conn
	writeMu sync.Mutex
	pending sync.Map
}

func (tc *testerConn) send(m stWSMsg) error {
	data, _ := json.Marshal(m)
	tc.writeMu.Lock()
	defer tc.writeMu.Unlock()
	return tc.conn.WriteMessage(websocket.TextMessage, data)
}

// SpeedTesterWSHandler 管理家用测速端连接。
type SpeedTesterWSHandler struct {
	repo     *storage.TrafficRepository
	upgrader websocket.Upgrader
	conns    sync.Map
}

func NewSpeedTesterWSHandler(repo *storage.TrafficRepository) *SpeedTesterWSHandler {
	return &SpeedTesterWSHandler{
		repo:     repo,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

func (h *SpeedTesterWSHandler) Online(testerID int64) bool {
	_, ok := h.conns.Load(testerID)
	return ok
}

func (h *SpeedTesterWSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	tester, err := h.repo.GetSpeedTesterByTokenHash(r.Context(), hashSpeedTesterToken(token))
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	tc := &testerConn{id: tester.ID, conn: conn}
	if old, ok := h.conns.Load(tester.ID); ok {
		old.(*testerConn).conn.Close()
	}
	h.conns.Store(tester.ID, tc)
	log.Printf("[SpeedTester] tester %d (%s) connected", tester.ID, tester.Name)
	h.repo.TouchSpeedTester(context.Background(), tester.ID)

	defer func() {
		conn.Close()
		if cur, ok := h.conns.Load(tester.ID); ok && cur.(*testerConn) == tc {
			h.conns.Delete(tester.ID)
		}
		log.Printf("[SpeedTester] tester %d disconnected", tester.ID)
	}()

	conn.SetReadLimit(64 * 1024)
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg stWSMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "result":
			if ch, ok := tc.pending.Load(msg.JobID); ok {
				select {
				case ch.(chan stWSMsg) <- msg:
				default:
				}
			}
		case "hello", "ping":
			h.repo.TouchSpeedTester(context.Background(), tester.ID)
			_ = tc.send(stWSMsg{Type: "pong"})
		}
	}
}

// Dispatch 把测速任务派给指定在线测速端，阻塞等结果。
// latencyOnly=true 时只测真延迟(Cloudflare 204),跳过下载。
func (h *SpeedTesterWSHandler) Dispatch(ctx context.Context, testerID int64, clashConfig string, bytes int64, url string, threads int, latencyOnly bool) (speedtest.Result, error) {
	v, ok := h.conns.Load(testerID)
	if !ok {
		return speedtest.Result{}, errors.New("测速端不在线")
	}
	tc := v.(*testerConn)
	jobID := uuid.New().String()
	ch := make(chan stWSMsg, 1)
	tc.pending.Store(jobID, ch)
	defer tc.pending.Delete(jobID)

	if err := tc.send(stWSMsg{
		Type: "run", JobID: jobID, ClashConfig: clashConfig,
		Bytes: bytes, URL: url, Threads: threads, LatencyOnly: latencyOnly,
	}); err != nil {
		return speedtest.Result{}, errors.New("下发任务失败: " + err.Error())
	}

	select {
	case res := <-ch:
		if res.Status != "ok" {
			return speedtest.Result{LatencyMs: res.LatencyMs, EgressIP: res.EgressIP}, errors.New(res.Error)
		}
		return speedtest.Result{DownMbps: res.DownMbps, LatencyMs: res.LatencyMs, Bytes: bytes, EgressIP: res.EgressIP}, nil
	case <-time.After(120 * time.Second):
		return speedtest.Result{}, errors.New("测速端响应超时")
	case <-ctx.Done():
		return speedtest.Result{}, ctx.Err()
	}
}
