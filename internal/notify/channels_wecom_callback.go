package notify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
)

// ==================== 企业微信回调（官方 WXBizMsgCrypt 协议，纯标准库） ====================
//
// AES-256-CBC，key = Base64Decode(EncodingAESKey + "=")，IV = key 前 16 字节；
// 明文结构：16 字节随机 + 4 字节网络序长度 + XML + corpid；
// 签名：sha1(字典序排序后的 token/timestamp/nonce/encrypt 拼接)。

// WecomCallback 处理企业微信「接收消息服务器配置」的 GET 验证与 POST 消息。
// onMessage 在独立 goroutine 里调用（企微要求 5 秒内应答），参数为发送者 ID 与文本指令。
// 路由本身不带鉴权：协议自带 Token 签名 + AES 加密验证。
func WecomCallback(cfg WecomConfig, onMessage func(user, text string)) http.Handler {
	return WecomCallbackWithLogger(cfg, onMessage, log.Default())
}

// WecomCallbackWithLogger 同 WecomCallback，可注入日志（nil 关闭）。
func WecomCallbackWithLogger(cfg WecomConfig, onMessage func(user, text string), logger Logger) http.Handler {
	key, keyErr := newWecomAESKey(cfg.EncodingAESKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logf := func(format string, args ...any) {
			if logger != nil {
				logger.Printf(format, args...)
			}
		}
		if keyErr != nil {
			logf("[notify] 企微回调 AES key 无效: %v", keyErr)
			http.Error(w, "aes key invalid", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		sig, ts, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")

		if r.Method == http.MethodGet {
			echo := q.Get("echostr")
			if !VerifyWecomSignature(cfg.Token, ts, nonce, echo, sig) {
				http.Error(w, "sign mismatch", http.StatusForbidden)
				return
			}
			plain, err := key.Decrypt(echo)
			if err != nil {
				http.Error(w, "decrypt fail", http.StatusBadRequest)
				return
			}
			_, _ = w.Write(plain)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var env struct {
			XMLName xml.Name `xml:"xml"`
			Encrypt string   `xml:"Encrypt"`
		}
		if xml.Unmarshal(raw, &env) != nil || env.Encrypt == "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success")) // 格式异常也应答，防企微重试轰炸
			return
		}
		if !VerifyWecomSignature(cfg.Token, ts, nonce, env.Encrypt, sig) {
			logf("[notify] 企微回调签名不对，已丢弃：检查 Token 是否与后台一致")
			http.Error(w, "sign mismatch", http.StatusForbidden)
			return
		}
		plain, err := key.Decrypt(env.Encrypt)
		if err != nil {
			logf("[notify] 企微回调解密失败: %v", err)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success"))
			return
		}
		var msg struct {
			FromUser string `xml:"FromUserName"`
			MsgType  string `xml:"MsgType"`
			Content  string `xml:"Content"`
			Event    string `xml:"Event"`
			EventKey string `xml:"EventKey"`
		}
		if xml.Unmarshal(plain, &msg) != nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("success"))
			return
		}
		text := strings.TrimSpace(msg.Content)
		if msg.MsgType == "event" && msg.Event == "click" {
			text = strings.TrimSpace(msg.EventKey) // 自定义菜单按钮
		}
		if (msg.MsgType == "text" || msg.MsgType == "event") && text != "" && onMessage != nil {
			user := msg.FromUser
			logf("[notify] 企微 ← %s: %s", user, truncate(text, 60))
			go onMessage(user, text)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("success"))
	})
}

// VerifyWecomSignature 校验企微回调签名（常量时间比较）。
func VerifyWecomSignature(token, timestamp, nonce, encrypt, signature string) bool {
	want := wecomSign(token, timestamp, nonce, encrypt)
	return subtleCompare(want, signature)
}

func wecomSign(token, timestamp, nonce, encrypt string) string {
	parts := []string{token, timestamp, nonce, encrypt}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// WecomAESKey 企业微信会话密钥。
type WecomAESKey []byte

// NewWecomAESKey 由 43 位 EncodingAESKey 生成密钥。
func NewWecomAESKey(encodingKey string) (WecomAESKey, error) {
	return newWecomAESKey(encodingKey)
}

func newWecomAESKey(encodingKey string) (WecomAESKey, error) {
	if len(encodingKey) != 43 {
		return nil, fmt.Errorf("EncodingAESKey 长度应为 43，实际 %d", len(encodingKey))
	}
	key, err := base64.StdEncoding.DecodeString(encodingKey + "=")
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("AES key 解码后应为 32 字节，实际 %d", len(key))
	}
	return key, nil
}

// Decrypt 解密企微密文，返回明文 XML。
func (k WecomAESKey) Decrypt(b64Ciphertext string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(b64Ciphertext)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < 32 || len(ciphertext)%16 != 0 {
		return nil, errors.New("密文长度非法")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, k[:16]).CryptBlocks(plain, ciphertext)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > 32 || pad > len(plain) {
		return nil, errors.New("PKCS7 填充非法")
	}
	plain = plain[:len(plain)-pad]
	if len(plain) < 20 {
		return nil, errors.New("明文过短")
	}
	msgLen := int(binary.BigEndian.Uint32(plain[16:20]))
	if msgLen < 0 || 20+msgLen > len(plain) {
		return nil, errors.New("消息长度字段越界")
	}
	return plain[20 : 20+msgLen], nil
}
