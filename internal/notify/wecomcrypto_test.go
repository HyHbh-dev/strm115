package notify

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// makeWecomCiphertext 按官方协议手工构造一份密文：
// 16 随机 + 4 字节网络序长度 + XML + corpid，PKCS7（块长 32），AES-256-CBC。
func makeWecomCiphertext(t *testing.T, keyBytes []byte, xmlPlain string) (encodingKey, b64 string) {
	t.Helper()
	encodingKey = strings.TrimSuffix(base64.StdEncoding.EncodeToString(keyBytes), "=")
	if len(encodingKey) != 43 {
		t.Fatalf("测试密钥长度应为 43，实际 %d", len(encodingKey))
	}
	plain := make([]byte, 16)
	copy(plain, []byte("0123456789ABCDEF"))
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(xmlPlain)))
	plain = append(plain, lenBuf...)
	plain = append(plain, []byte(xmlPlain)...)
	plain = append(plain, []byte("corp-test")...)
	pad := 32 - len(plain)%32
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, keyBytes[:16]).CryptBlocks(out, plain)
	return encodingKey, base64.StdEncoding.EncodeToString(out)
}

func TestWecomAESRoundTripAndSignature(t *testing.T) {
	keyBytes := bytes.Repeat([]byte{0x2A}, 32)
	const xmlPlain = `<xml><Content>搜索 沙丘</Content></xml>`
	encodingKey, b64 := makeWecomCiphertext(t, keyBytes, xmlPlain)

	key, err := NewWecomAESKey(encodingKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := key.Decrypt(b64)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != xmlPlain {
		t.Fatalf("明文不符: %q", got)
	}
	if _, err := NewWecomAESKey("too-short"); err == nil {
		t.Fatal("非法长度密钥应报错")
	}
	if _, err := key.Decrypt("!!!!not-base64"); err == nil {
		t.Fatal("非法密文应报错")
	}

	const token, ts, nonce = "TOKEN", "1700000000", "abc123"
	sig := wecomSign(token, ts, nonce, b64)
	if !VerifyWecomSignature(token, ts, nonce, b64, sig) {
		t.Fatal("正确签名校验失败")
	}
	if VerifyWecomSignature(token, ts, nonce, b64, "bad") {
		t.Fatal("错误签名不该通过")
	}
	if VerifyWecomSignature("wrong-token", ts, nonce, b64, sig) {
		t.Fatal("错误 Token 不该通过")
	}
}

func TestWecomCallbackGETAndPOST(t *testing.T) {
	keyBytes := bytes.Repeat([]byte{0x37}, 32)
	const xmlPlain = `<xml><Content>整理</Content><FromUserName>alice</FromUserName><MsgType>text</MsgType></xml>`
	encodingKey, b64 := makeWecomCiphertext(t, keyBytes, xmlPlain)
	cfg := WecomConfig{Token: "TOKEN", EncodingAESKey: encodingKey}
	h := WecomCallbackWithLogger(cfg, nil, nil)

	const ts, nonce = "1700000000", "n1"
	sig := wecomSign("TOKEN", ts, nonce, b64)
	q := url.Values{
		"msg_signature": {sig}, "timestamp": {ts}, "nonce": {nonce}, "echostr": {b64},
	}

	// GET：URL 验证，应回显解密后的明文
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/wecom/callback?"+q.Encode(), nil))
	if w.Code != 200 || w.Body.String() != xmlPlain {
		t.Fatalf("GET 验证失败: %d %q", w.Code, w.Body.String())
	}

	// GET：签名错误应 403
	bad := url.Values{
		"msg_signature": {"wrong"}, "timestamp": {ts}, "nonce": {nonce}, "echostr": {b64},
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/wecom/callback?"+bad.Encode(), nil))
	if w.Code != 403 {
		t.Fatalf("错误签名应 403，得到 %d", w.Code)
	}

	// POST：正常消息，异步交给 onMessage
	q.Del("echostr")
	q.Set("msg_signature", wecomSign("TOKEN", ts, nonce, b64))
	body := `<xml><Encrypt>` + b64 + `</Encrypt></xml>`
	got := make(chan string, 1)
	h2 := WecomCallbackWithLogger(cfg, func(user, text string) {
		got <- user + ":" + text
	}, nil)
	w = httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest("POST", "/wecom/callback?"+q.Encode(), strings.NewReader(body)))
	if w.Code != 200 || w.Body.String() != "success" {
		t.Fatalf("POST 应答异常: %d %q", w.Code, w.Body.String())
	}
	select {
	case v := <-got:
		if v != "alice:整理" {
			t.Fatalf("指令解析错误: %q", v)
		}
	case <-time.After(time.Second):
		t.Fatal("onMessage 未被调用")
	}
}
