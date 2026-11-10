// Package pan defines the only boundary through which this program is allowed
// to touch a cloud drive.
//
// 设计要点（改动前请先读）：
//
//  1. 业务代码（同步、整理、播放）只认 Driver 接口，不认任何网盘 SDK。
//     将来接夸克/阿里，只加一个实现 + 一次 Register，不动业务。
//
//  2. 所有调用都必须经过 Manager。Manager 负责节流、串行、重试、熔断、审计——
//     这些是「防封号」的全部手段，绕过 Manager 直接拿裸 Driver 发请求 =
//     风控风险全部回到业务代码里，那是这个项目唯一不能接受的耦合。
//
//  3. 接口刻意做成「一页一页列」而不是「一次列全」。
//     网盘 SDK 自带的 List 通常内部循环翻页，中途无法插入等待；
//     由 Manager 控制翻页节奏，才能保证任意两次请求之间都有间隔。
package pan

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Entry 是网盘条目的中立表示。各网盘字段差异在各自的 driver 里吃掉。
type Entry struct {
	ID       string    // 全局唯一 id（115 是 fid）
	ParentID string    // 父目录 id
	Name     string    // 文件名
	IsDir    bool      // 是否目录
	Size     int64     // 字节数，目录为 0
	PickCode string    // 下载标识（115 的 pickcode），目录可能为空
	SHA1     string    // 内容哈希，可空
	ModTime  time.Time // 修改时间，零值表示未知
}

// Account 是「一个已登录的网盘账号」。
// Credential 是不透明字符串，只由对应 driver 解释（115 存的是 UID/CID/SEID/KID 的 JSON）。
type Account struct {
	ID         int64
	Kind       string // 115 / quark / …
	Name       string
	Credential string
}

// OfflineStatus 是离线下载任务的归一状态。
//
// 各网盘的数字状态码含义不同（115 是 0/1/2/-1），归一成下面四个值之后，
// 上层（提示、通知、前端标签）就不用关心是哪个网盘了。
type OfflineStatus string

const (
	OfflineWaiting OfflineStatus = "waiting"
	OfflineRunning OfflineStatus = "running"
	OfflineDone    OfflineStatus = "done"
	OfflineFailed  OfflineStatus = "failed"
)

// OfflineTask 是网盘离线下载任务的中立表示。
type OfflineTask struct {
	Hash    string        `json:"hash"`               // 任务标识（115 用 info_hash）
	Name    string        `json:"name,omitempty"`     // 文件名（部分网盘完成后才有）
	Size    int64         `json:"size"`               // 字节数
	Status  OfflineStatus `json:"status"`             // 归一状态
	Percent float64       `json:"percent"`            // 0~100
	FileID  string        `json:"file_id,omitempty"`  // 完成后落盘的条目 id，可空
	DirID   string        `json:"dir_id,omitempty"`   // 保存目录 id
	AddTime time.Time     `json:"add_time,omitempty"` // 提交时间，零值表示未知
}

// OfflineDownloader 是「可选能力」：把磁力 / ed2k / 种子链接交给网盘自己下载。
//
// 之所以不做进 Driver 接口，是因为不是所有网盘都有离线下载；用一个可选接口
// + Manager 上的类型断言，新增网盘时不必凭空实现一堆返回「不支持」的方法。
//
// 实现方和 Driver 一样：只做字段翻译与错误归类，不做重试与节流。
type OfflineDownloader interface {
	// AddOfflineURIs 提交一批链接，返回每个链接对应的任务标识。
	AddOfflineURIs(ctx context.Context, uris []string, saveDirID string) ([]string, error)
	// ListOfflineTasks 列一页任务，返回任务列表与总数（未知时 -1）。
	ListOfflineTasks(ctx context.Context, page int64) ([]OfflineTask, int64, error)
}

// ShareSaveResult 是一次「分享转存」的结果。
//
// Already 单独标出来，是因为 115 对同一份分享再转存一次不是失败，而是
// errno 4100024「已经转存过」—— 对用户来说这是「文件早就在网盘里了」，
// 前端该提示一句而不是报错。
type ShareSaveResult struct {
	Title   string `json:"title,omitempty"`   // 分享标题（网盘给的，可能是原文件名）
	Files   int    `json:"files"`             // 转存了几个顶层条目
	Already bool   `json:"already,omitempty"` // 之前就转存过
}

// ShareSaver 是「可选能力」：把别人的分享链接转存进自己的空间。
//
// 和离线下载一样不放进 Driver：不是所有网盘都有分享，也不是所有网盘都值得做。
// 目前只有 115 实现（TG 资源帖 99% 是 115 分享）。
type ShareSaver interface {
	// SaveShare 把 shareCode 对应的分享转到 toDirID。
	// password 是提取码，没有就传空；toDirID 传空表示根目录。
	SaveShare(ctx context.Context, shareCode, password, toDirID string) (ShareSaveResult, error)
}

// Driver 是网盘能力的全集。新增网盘 = 实现这个接口。

// 约定：
//   - 所有方法都必须尊重 ctx.Done()，且不自己做重试（重试归 Manager）。
//   - 出错时尽量返回下面的哨兵错误，便于 Manager 分类。
type Driver interface {
	Kind() string

	// ListPage 列 dirID 下的一页。返回本页条目与总数（总数未知时返回 -1）。
	// offset 为 0 表示第一页。
	ListPage(ctx context.Context, dirID string, offset, limit int) ([]Entry, int, error)

	// Stat 查单个条目（用于校验 pickcode / 改名后回读）。
	Stat(ctx context.Context, id string) (Entry, error)

	// DownloadURL 取播放直链。ua 必须原样透传给网盘——直链是按 UA 签发的，
	// 用别的 UA 取到的链接在播放器上会 403。
	DownloadURL(ctx context.Context, pickCode, ua string) (string, error)

	// Mkdir 建目录，返回新目录 id。
	Mkdir(ctx context.Context, parentID, name string) (string, error)

	// Move 把 ids 移到 toDirID。
	Move(ctx context.Context, toDirID string, ids ...string) error

	// Rename 改名。
	Rename(ctx context.Context, id, name string) error

	// Remove 删除（进回收站，不是物理删除）。
	Remove(ctx context.Context, ids ...string) error

	// Ping 轻量校验凭证是否还有效。不能有副作用（115 的 LoginCheck 会踢掉其它设备，不许用它）。
	Ping(ctx context.Context) error

	Close() error
}

// Opener 由各 driver 注册。acct.Credential 已由调用方解密/取出。
type Opener func(ctx context.Context, acct Account) (Driver, error)

var openers = map[string]Opener{}

// Register 注册一个网盘实现。只在 init 里调用。
func Register(kind string, o Opener) {
	if _, dup := openers[kind]; dup {
		panic("pan: duplicate driver kind " + kind)
	}
	openers[kind] = o
}

// Kinds 返回已注册的网盘种类。
func Kinds() []string {
	out := make([]string, 0, len(openers))
	for k := range openers {
		out = append(out, k)
	}
	return out
}

// Open 用账号凭证建一个 driver。返回的是裸 driver，调用方必须再包一层 Manager。
func Open(ctx context.Context, acct Account) (Driver, error) {
	o, ok := openers[acct.Kind]
	if !ok {
		return nil, fmt.Errorf("pan: 未注册的网盘类型 %q（已注册：%v）", acct.Kind, Kinds())
	}
	return o(ctx, acct)
}

// ==================== 错误分类 ====================
//
// Manager 靠这几类决定「要不要重试 / 要不要熔断」，所以 driver 必须如实归类：
// 把风控错误当普通错误重试，就是在帮网盘封自己的号。

var (
	// ErrCooling 熔断中：调用被本地拒绝，一个请求都没有发出去。
	ErrCooling = errors.New("网盘接口冷却中")
	// ErrNotFound 目标不存在。
	ErrNotFound = errors.New("目标不存在")
	// ErrAuth 凭证失效，需要重新登录。
	ErrAuth = errors.New("凭证失效")
	// ErrTransient 临时故障（网络、超时、5xx），可重试。
	ErrTransient = errors.New("临时故障")
	// ErrRisk 被风控（频率过高 / 服务器开小差）。绝不可立即重试。
	ErrRisk = errors.New("触发风控")
	// ErrUnsupported 该网盘不支持这个操作（比如没有离线下载）。重试无意义。
	ErrUnsupported = errors.New("该网盘不支持此操作")
	// ErrRejected 网盘明确拒绝了这次请求：链接无效、任务已存在、超出配额…
	// 请求本身有问题，原样重试只会得到同样的拒绝。
	ErrRejected = errors.New("网盘拒绝了该请求")
)

// Error 携带原始错误与分类。
type Error struct {
	Kind error  // 上面某个哨兵
	Op   string // 操作名，便于日志
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	msg := e.Msg
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = e.Kind.Error()
	}
	return e.Op + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is 支持 errors.Is(err, pan.ErrRisk) 这样的判断。
func (e *Error) Is(target error) bool { return target == e.Kind }

// NewErr 构造一个带分类的错误。各网盘 driver 用它把自家错误翻译成统一分类。
func NewErr(kind error, op, msg string, err error) error {
	return &Error{Kind: kind, Op: op, Msg: msg, Err: err}
}

func wrap(kind error, op, msg string, err error) error { return NewErr(kind, op, msg, err) }

// KindOf 取出错误分类；认不出来按临时故障处理。
func KindOf(err error) error {
	if err == nil {
		return nil
	}
	for _, k := range []error{ErrCooling, ErrNotFound, ErrAuth, ErrRisk, ErrUnsupported, ErrRejected, ErrTransient} {
		if errors.Is(err, k) {
			return k
		}
	}
	return ErrTransient
}
