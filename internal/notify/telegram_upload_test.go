package notify

// Telegram 本地图片直传。
//
// 这条路径是「家用 NAS 也能发封面」的关键：海报在磁盘上，没有公网地址，
// 只能读字节上传。测试要证明两件事：真的走了 multipart/sendPhoto，
// 图读不出来时能干净地退回纯文本（而不是整条消息丢掉）。

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelegramUploadLocalPhoto(t *testing.T) {
	dir := t.TempDir()
	poster := filepath.Join(dir, "poster.jpg")
	if err := os.WriteFile(poster, []byte("\xff\xd8\xffJPEGBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	var hitPhoto, hitText int
	var photoCT, photoBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			hitPhoto++
			photoCT = r.Header.Get("Content-Type")
			b, _ := io.ReadAll(r.Body)
			photoBody = string(b)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			hitText++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer ts.Close()

	old := telegramAPIBase
	telegramAPIBase = ts.URL
	t.Cleanup(func() { telegramAPIBase = old })

	cfg := Config{TG: TGConfig{Enabled: Switch(true), Token: "T", ChatID: "42"}}
	err := telegramChannel{}.SendRich(context.Background(), &cfg, ts.Client(), Message{
		Kind:      KindIngest,
		Title:     "🆕 空枪（2026）",
		Content:   "🎬 电影 · ⭐ 7.2",
		ImageFile: poster,
	})
	if err != nil {
		t.Fatalf("上传封面失败: %v", err)
	}
	if hitPhoto != 1 || hitText != 0 {
		t.Fatalf("应只走 sendPhoto，实际 photo=%d text=%d", hitPhoto, hitText)
	}
	mt, _, _ := mime.ParseMediaType(photoCT)
	if mt != "multipart/form-data" {
		t.Fatalf("应是 multipart 上传，实际 %q", photoCT)
	}
	for _, want := range []string{`name="chat_id"`, "42", `name="photo"`, "poster.jpg", "JPEGBYTES", "空枪"} {
		if !strings.Contains(photoBody, want) {
			t.Fatalf("上传体缺 %q", want)
		}
	}
}

func TestTelegramUploadFallsBackToText(t *testing.T) {
	var hitText int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			hitText++
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	old := telegramAPIBase
	telegramAPIBase = ts.URL
	t.Cleanup(func() { telegramAPIBase = old })

	cfg := Config{TG: TGConfig{Enabled: Switch(true), Token: "T", ChatID: "42"}}
	err := telegramChannel{}.SendRich(context.Background(), &cfg, ts.Client(), Message{
		Kind:      KindIngest,
		Title:     "🆕 空枪（2026）",
		Content:   "🎬 电影",
		ImageFile: filepath.Join(t.TempDir(), "missing.jpg"),
	})
	// 正文照发，但要把「图没发出去」带出来：不能既退回纯文本又假装一切正常。
	var fb *FallbackError
	if !errors.As(err, &fb) {
		t.Fatalf("图读不到时应退回文本并返回 FallbackError，实际 %v", err)
	}
	if hitText != 1 {
		t.Fatalf("应退回 sendMessage，实际 %d 次", hitText)
	}
}

// 回归：入库卡片只带 ImageFile（本地 poster.jpg），不带公网 Image。
// 客户端以前只判断 m.Image，于是 SendRich 永远不被调用 —— 卡片有海报文件却
// 只剩文字。这个测试盯的是「扇出层」，不是渠道层。
func TestClientRichUsesLocalImageFile(t *testing.T) {
	dir := t.TempDir()
	poster := filepath.Join(dir, "poster.jpg")
	if err := os.WriteFile(poster, []byte("JPEGBYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	var photo, text int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			photo++
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			text++
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	old := telegramAPIBase
	telegramAPIBase = ts.URL
	t.Cleanup(func() { telegramAPIBase = old })

	cfg := Config{TG: TGConfig{Enabled: Switch(true), Token: "T", ChatID: "42"}}
	c := NewWithDoer(cfg, ts.Client(), nil)
	res := c.SendRich(context.Background(), Message{
		Kind: KindIngest, Title: "🆕 空枪（2026）", Content: "🎬 电影", ImageFile: poster,
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("应发送成功，得到 %+v", res)
	}
	if photo != 1 || text != 0 {
		t.Fatalf("只给本地海报时应走 sendPhoto，实际 photo=%d text=%d", photo, text)
	}
}

// 图挂掉时要退回纯文本，且整条消息仍算「发出去了」——不能让日志里出现
// 「通知发送失败」，那会把「图没传上去」和「消息没发出去」混成一件事。
func TestClientRichFallbackStaysOK(t *testing.T) {
	var photo, text int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"):
			photo++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"wrong file identifier"}`))
			return
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			text++
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	old := telegramAPIBase
	telegramAPIBase = ts.URL
	t.Cleanup(func() { telegramAPIBase = old })

	cfg := Config{TG: TGConfig{Enabled: Switch(true), Token: "T", ChatID: "42"}}
	c := NewWithDoer(cfg, ts.Client(), nil)
	res := c.SendRich(context.Background(), Message{
		Kind: KindIngest, Title: "🆕 空枪（2026）", Content: "🎬 电影",
		ImageFile: filepath.Join(t.TempDir(), "missing.jpg"),
	})
	if len(res) != 1 || !res[0].OK {
		t.Fatalf("退回纯文本仍算成功，得到 %+v", res)
	}
	if res[0].Error != "" {
		t.Errorf("降级不该记成发送失败: %q", res[0].Error)
	}
	if text != 1 || photo != 0 {
		t.Fatalf("应只发一条纯文本，实际 text=%d photo=%d", text, photo)
	}
}
