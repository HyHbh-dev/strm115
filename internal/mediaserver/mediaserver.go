// Package mediaserver 抽象「媒体服务器」这一侧的能力。
//
// 为什么要抽象：Emby 和 Jellyfin 是同一套 API 的两个分支，而现在的要求是
// 「网盘的增删改查要同步到 Emby，Emby 的操作也要反过来影响网盘」——
// 后者意味着以后一定会加 Webhook 接收和条目级对账，那些逻辑不该知道
// 自己面对的是 Emby 还是 Jellyfin。
package mediaserver

import (
	"context"
	"errors"
)

// Library 是媒体服务器上的一个库。
type Library struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	CollectionType string   `json:"collection_type"`
	Paths          []string `json:"paths"`
}

// Server 是媒体服务器的最小能力集（v1 只需要「建库」和「刷新」）。
type Server interface {
	Name() string
	Ping(ctx context.Context) error
	Libraries(ctx context.Context) ([]Library, error)
	// EnsureLibrary 幂等：同名库已存在则直接返回，不存在才创建。
	EnsureLibrary(ctx context.Context, name, collectionType string, paths []string) (Library, error)
	// Refresh 触发某个库的扫描。libID 为空表示刷新全库。
	Refresh(ctx context.Context, libID string) error
	// RefreshPaths 让服务器重读这些本地路径对应的条目（.nfo / 图片变了时用）。
	// 与 Refresh 的区别：整库扫描不会重读「已入库但内容变了」的 nfo。
	// 返回实际刷新的条目数。
	RefreshPaths(ctx context.Context, paths []string) (int, error)
	// MediaUpdated 把「这几个文件被改过了」直接告诉服务器。
	//
	// 光调 RefreshPaths 不够：条目刷新时 Emby 会先看主媒体文件（.strm）的
	// mtime 有没有变，没变就可能整段跳过 ffprobe，外挂字幕流索引还是旧的。
	// /Library/Media/Updated 是 Emby 官方给外部程序用的「文件变了」通知，
	// 传真实文件路径，服务器自己决定扫哪个库。
	MediaUpdated(ctx context.Context, paths []string) error
}

// Config 是连接参数。
type Config struct {
	Kind   string // emby / jellyfin
	URL    string // http://192.168.1.10:8096
	APIKey string
}

var ErrDisabled = errors.New("未配置媒体服务器")

// New 根据配置构造实现；配置不全时返回 ErrDisabled，调用方按「没配」处理。
func New(cfg Config) (Server, error) {
	if cfg.URL == "" || cfg.APIKey == "" {
		return nil, ErrDisabled
	}
	return newEmby(cfg), nil
}
