package netproxy

import "testing"

func withFakeContainer(t *testing.T, host string) {
	t.Helper()
	oldCheck, oldHost := containerCheck, hostLookup
	containerCheck = func() bool { return true }
	hostLookup = func() string { return host }
	t.Cleanup(func() { containerCheck, hostLookup = oldCheck, oldHost })
}

func TestNormalizeRewritesLoopbackInContainer(t *testing.T) {
	withFakeContainer(t, "172.21.0.1")
	cases := map[string]string{
		"http://127.0.0.1:7890":         "http://172.21.0.1:7890",
		"http://localhost:7890":         "http://172.21.0.1:7890",
		"socks5://127.0.0.1:1080":       "socks5://172.21.0.1:1080",
		"http://user:pw@127.0.0.1:8080": "http://user:pw@172.21.0.1:8080",
		"http://127.0.0.1":              "http://172.21.0.1:80",
		// 已经填对的 LAN IP / 域名不该被动
		"http://192.168.1.10:7890": "http://192.168.1.10:7890",
		"http://proxy.local:7890":   "http://proxy.local:7890",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeKeepsOriginalWhenNoGateway(t *testing.T) {
	withFakeContainer(t, "")
	if got := Normalize("http://127.0.0.1:7890"); got != "http://127.0.0.1:7890" {
		t.Errorf("拿不到网关时该原样返回，实际 %q", got)
	}
}

func TestNormalizeNoopOutsideContainer(t *testing.T) {
	old := containerCheck
	containerCheck = func() bool { return false }
	t.Cleanup(func() { containerCheck = old })
	if got := Normalize("http://127.0.0.1:7890"); got != "http://127.0.0.1:7890" {
		t.Errorf("宿主机直跑不该改写，实际 %q", got)
	}
}
