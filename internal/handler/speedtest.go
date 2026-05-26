package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"miaomiaowu/internal/auth"
	"miaomiaowu/internal/speedtest"
	"miaomiaowu/internal/storage"
)

type SpeedTestHandler struct {
	repo     *storage.TrafficRepository
	testerWS *SpeedTesterWSHandler
}

func NewSpeedTestHandler(repo *storage.TrafficRepository) *SpeedTestHandler {
	return &SpeedTestHandler{repo: repo}
}

func (h *SpeedTestHandler) SetTesterWS(t *SpeedTesterWSHandler) { h.testerWS = t }

func (h *SpeedTestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/admin/speedtest/run" && r.Method == http.MethodPost:
		h.handleRun(w, r)
	case r.URL.Path == "/api/admin/speedtest/results" && r.Method == http.MethodGet:
		h.handleResults(w, r)
	case r.URL.Path == "/api/admin/speedtest/mihomo-status" && r.Method == http.MethodGet:
		ready, path := speedtest.MihomoStatus()
		respondJSON(w, http.StatusOK, map[string]any{"success": true, "ready": ready, "path": path})
	case r.URL.Path == "/api/admin/speedtest/testers" && r.Method == http.MethodGet:
		h.handleTestersList(w, r)
	case r.URL.Path == "/api/admin/speedtest/testers/create" && r.Method == http.MethodPost:
		h.handleTesterCreate(w, r)
	case r.URL.Path == "/api/admin/speedtest/testers/revoke" && r.Method == http.MethodPost:
		h.handleTesterRevoke(w, r)
	default:
		writeError(w, http.StatusNotFound, errors.New("not found"))
	}
}

func (h *SpeedTestHandler) handleRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID      int64  `json:"node_id"`
		Bytes       int64  `json:"bytes,omitempty"`
		URL         string `json:"url,omitempty"`
		TesterID    int64  `json:"tester_id,omitempty"`
		Threads     int    `json:"threads,omitempty"`
		LatencyOnly bool   `json:"latency_only,omitempty"` // true 仅测真连接延迟(Cloudflare 204)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeID <= 0 {
		writeBadRequest(w, "node_id 必填")
		return
	}
	ctx := r.Context()
	node, err := h.repo.GetNodeByID(ctx, req.NodeID)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("节点不存在"))
		return
	}
	if node.ClashConfig == "" {
		writeBadRequest(w, "该节点无 clash 配置，无法测速")
		return
	}

	source := "master_local"
	if req.TesterID > 0 {
		if h.testerWS == nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("家用测速端未启用"))
			return
		}
		source = "home_tester"
	}

	rec := storage.SpeedTestResult{
		NodeID:    node.ID,
		NodeName:  node.NodeName,
		Source:    source,
		TestBytes: req.Bytes,
		TestedBy:  auth.UsernameFromContext(ctx),
		Status:    "running",
	}
	id, ierr := h.repo.InsertSpeedTestResult(ctx, rec)
	if ierr != nil {
		writeError(w, http.StatusInternalServerError, ierr)
		return
	}
	rec.ID = id
	rec.CreatedAt = time.Now()

	go h.runSpeedTestAsync(id, req.TesterID, node.ClashConfig, req.Bytes, req.URL, req.Threads, req.LatencyOnly)

	respondJSON(w, http.StatusOK, map[string]any{"success": true, "result": rec})
}

func (h *SpeedTestHandler) runSpeedTestAsync(recID, testerID int64, clashConfig string, bytes int64, url string, threads int, latencyOnly bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var res speedtest.Result
	var terr error
	if testerID > 0 {
		res, terr = h.testerWS.Dispatch(ctx, testerID, clashConfig, bytes, url, threads, latencyOnly)
	} else {
		bin, merr := speedtest.EnsureMihomo(ctx)
		if merr != nil {
			terr = merr
		} else {
			res, terr = speedtest.RunNodeTest(ctx, bin, clashConfig, speedtest.Options{
				TestBytes: bytes, TestURL: url, Threads: threads, LatencyOnly: latencyOnly,
			})
		}
	}

	status, errMsg := "ok", ""
	if terr != nil {
		status, errMsg = "failed", terr.Error()
	}
	_ = h.repo.UpdateSpeedTestResult(ctx, recID, res.DownMbps, res.LatencyMs, res.Bytes, status, errMsg, res.EgressIP)
}

func (h *SpeedTestHandler) handleTesterCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	token := hex.EncodeToString(buf)
	id, err := h.repo.CreateSpeedTester(r.Context(), req.Name, hashSpeedTesterToken(token), auth.UsernameFromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "token": token})
}

func (h *SpeedTestHandler) handleTestersList(w http.ResponseWriter, r *http.Request) {
	list, err := h.repo.ListSpeedTesters(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		online := h.testerWS != nil && h.testerWS.Online(t.ID)
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "created_by": t.CreatedBy,
			"last_seen": t.LastSeen, "created_at": t.CreatedAt, "online": online,
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"success": true, "testers": out})
}

func (h *SpeedTestHandler) handleTesterRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeBadRequest(w, "id 必填")
		return
	}
	if err := h.repo.DeleteSpeedTester(r.Context(), req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"success": true})
}

func (h *SpeedTestHandler) handleResults(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("latest") == "1" {
		list, err := h.repo.ListLatestSpeedTestResults(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"success": true, "results": list})
		return
	}
	nodeID, _ := strconv.ParseInt(r.URL.Query().Get("node_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.repo.ListSpeedTestResults(r.Context(), nodeID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"success": true, "results": list})
}

func hashSpeedTesterToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
