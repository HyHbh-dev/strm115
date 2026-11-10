package httpapi

// Emby 302 网关的管理侧胶水：设置一改就按新配置起停，并把状态回报给页面。

import (
	"context"
	"strings"

	"strm115/internal/embygw"
)

// applyGateway 按当前设置起停 302 网关。
//
// 失败只记日志：网关没起来最多是「播放继续走 Emby 中转」，不该连带
// 让保存设置本身报错 —— 用户改的是端口，不是业务数据。
func (s *Server) applyGateway(ctx context.Context) {
	if s.gw == nil {
		return
	}
	if err := embygw.ApplyFromStore(ctx, s.st, s.gw, nil, s.log); err != nil {
		s.log.Warn("Emby 302 网关未启用", "err", err)
	}
}

// gatewayStatus 是给设置页看的一行状态。
//
// listen 是播放端口的实际监听地址（网关开没开都在），addr 只在网关生效时有值 ——
// 两者合并成一个端口之后，页面要能说清「用的是哪个端口、网关有没有接管」。
func (s *Server) gatewayStatus() map[string]any {
	if s.gw == nil {
		return map[string]any{"enabled": false, "addr": "", "listen": ""}
	}
	return map[string]any{
		"enabled": s.gw.Addr() != "",
		"addr":    s.gw.Addr(),
		"listen":  s.gw.ListenAddr(),
	}
}

// migrateGatewayOn 把老配置里的「端口号」翻译成新配置里的「开关」。
//
// 合并端口之前，用户填了 18096 就等于启用了网关；升级后那个端口不再存在，
// 如果只看新键 emby_gateway_on，用户会莫名其妙地丢掉已开的功能。
// 只迁移一次（已有 on 就什么都不做），值本身也很简单：非空且不是 0/off 即开。
func (s *Server) migrateGatewayOn(ctx context.Context) {
	if _, ok := s.st.GetSetting(ctx, "emby_gateway_on"); ok {
		return
	}
	raw, ok := s.st.GetSetting(ctx, "emby_gateway_port")
	if !ok {
		return
	}
	on := "0"
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "off", "false", "no":
	default:
		on = "1"
	}
	_ = s.st.SetSetting(ctx, "emby_gateway_on", on)
}
