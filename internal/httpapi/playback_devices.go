package httpapi

// 同时播放设备：读设置里的上限、列出当前设备、允许手动释放。
//
// 115 账号同时开太多路容易被风控，参考 115-station 的 proxyAcctLimiter /
// proxy-status 做成「上限 + 正在播放列表」。差别是本站默认 302 直连，
// 字节不经过本机，所以会话按「最后一次起播 + 45 分钟」过期，不是按连接关闭。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"strm115/internal/playback"
)

// playMaxDevices 读用户设的上限。留空 / 非法值用默认 2；0 表示不限制。
func (s *Server) playMaxDevices(ctx context.Context) int {
	raw := strings.TrimSpace(s.setting(ctx, "play_max_devices"))
	if raw == "" {
		return playback.DefaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 16 {
		return playback.DefaultLimit
	}
	return n
}

// handlePlaybackDevices GET /api/playback/devices
func (s *Server) handlePlaybackDevices(w http.ResponseWriter, r *http.Request) {
	limit := s.playMaxDevices(r.Context())
	devices := playback.Default.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"limit":       limit,
		"unlimited":   limit == 0,
		"ttl_minutes": playback.Default.TTLMinutes(),
		"count":       len(devices),
		"full":        limit > 0 && len(devices) >= limit,
		"devices":     devices,
	})
}

// handlePlaybackDeviceRelease DELETE /api/playback/devices/{key}
// 手动踢掉一台：设备关掉播放器后服务端收不到通知，这一下能立刻把名额还回去。
func (s *Server) handlePlaybackDeviceRelease(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少设备标识"))
		return
	}
	ok := playback.Default.Release(key)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "key": key})
}
