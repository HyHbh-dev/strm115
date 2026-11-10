package linkparse

import "testing"

func TestParse115ShareFromURL(t *testing.T) {
	got := Parse115Share("名称：沙丘2 2024 2160p\n链接：https://115.com/s/swz8k9x3?password=ab12")
	if got == nil {
		t.Fatal("没认出 115 分享")
	}
	if got.ShareCode != "swz8k9x3" || got.Password != "ab12" {
		t.Fatalf("码/密码不对: %+v", got)
	}
	if got.Host != "115.com" || got.URL != "https://115.com/s/swz8k9x3?password=ab12" {
		t.Errorf("域名或链接不对: %+v", got)
	}
	if got.Matched != "url" {
		t.Errorf("matched 应为 url，实际 %q", got.Matched)
	}
}

func TestParse115SharePasswordOnNextLine(t *testing.T) {
	got := Parse115Share("115.com/s/swnp2i8\n提取码：k9d2\n解压密码：7788")
	if got == nil || got.ShareCode != "swnp2i8" {
		t.Fatalf("没认出分享码: %+v", got)
	}
	if got.Password != "k9d2" {
		t.Errorf("提取码应取 k9d2，实际 %q", got.Password)
	}
	if got.URL != "https://115.com/s/swnp2i8?password=k9d2" {
		t.Errorf("拼出来的链接不对: %s", got.URL)
	}
}

func TestParse115ShareOtherHosts(t *testing.T) {
	cases := map[string]string{
		"https://115cdn.com/s/swabc123": "115cdn.com",
		"https://anxia.com/s/swabc456":  "anxia.com",
		"http://www.115.com/s/swabc789": "115.com",
	}
	for in, host := range cases {
		got := Parse115Share(in)
		if got == nil {
			t.Fatalf("%s 没认出来", in)
		}
		if got.Host != host {
			t.Errorf("%s 域名应为 %s，实际 %s", in, host, got.Host)
		}
	}
}

func TestParse115ShareFromCodeOnly(t *testing.T) {
	got := Parse115Share("分享码：swz8k9x3 赶紧存")
	if got == nil {
		t.Fatal("只发分享码的情况没认出来")
	}
	if got.ShareCode != "swz8k9x3" || got.Matched != "code" {
		t.Errorf("分享码识别不对: %+v", got)
	}
	// 没给密码时 URL 不带 query，115 会自己弹输入框
	if got.URL != "https://115.com/s/swz8k9x3" {
		t.Errorf("无密码链接不该带 ?password=：%s", got.URL)
	}
}

func TestParse115SharesMultiple(t *testing.T) {
	text := "4K：https://115.com/s/swaaa111?password=aaaa\n1080p：https://115.com/s/swbbb222\n" +
		"重复：https://115.com/s/swaaa111?password=aaaa"
	all := Parse115Shares(text)
	if len(all) != 2 {
		t.Fatalf("应去重成 2 条，实际 %d: %+v", len(all), all)
	}
	// 正文没写提取码，第二条的密码就该空着
	if all[1].Password != "" {
		t.Errorf("第二条不该有密码: %+v", all[1])
	}
}

func TestParse115ShareIgnoresArchivePassword(t *testing.T) {
	got := Parse115Share("https://115.com/s/swz8k9x3\n解压密码：8800")
	if got == nil {
		t.Fatal("没认出分享")
	}
	if got.Password != "" {
		t.Errorf("解压密码不该当成分享密码: %q", got.Password)
	}
}

func TestParse115ShareWeakPassword(t *testing.T) {
	got := Parse115Share("https://115.com/s/swz8k9x3\n密码 abcd")
	if got == nil || got.Password != "abcd" {
		t.Errorf("弱标签密码没认出来: %+v", got)
	}
}

func TestParse115ShareNone(t *testing.T) {
	for _, in := range []string{
		"",
		"magnet:?xt=urn:btih:a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4",
		"https://pan.baidu.com/s/1abcdefg",
		"这是普通的一句话，没有网盘链接",
	} {
		if got := Parse115Share(in); got != nil {
			t.Errorf("%q 不该认出 115 分享: %+v", in, got)
		}
	}
}
