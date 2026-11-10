package httpapi

import (
	"strings"
	"testing"
)

// 转存接口接受三种入参写法，前端两种都会传，这里把收敛规则钉住。
func TestShareInput(t *testing.T) {
	cases := []struct {
		name       string
		url, code  string
		pwd        string
		wantCode   string
		wantPwd    string
		wantErrSub string
	}{
		{
			name:     "链接带提取码",
			url:      "https://115.com/s/swz8k9x3?password=ab12",
			wantCode: "swz8k9x3",
			wantPwd:  "ab12",
		},
		{
			name:     "链接 + 单独传的提取码",
			url:      "https://115cdn.com/s/swz8k9x3",
			pwd:      "k9d2",
			wantCode: "swz8k9x3",
			wantPwd:  "k9d2",
		},
		{
			name:     "直接给码",
			code:     "swnp2i8",
			pwd:      "abcd",
			wantCode: "swnp2i8",
			wantPwd:  "abcd",
		},
		{
			name:     "整段文案当链接传（前端偷懒）",
			url:      "名称：沙丘2\n链接：https://115.com/s/swz8k9x3 提取码：zx98",
			wantCode: "swz8k9x3",
			wantPwd:  "zx98",
		},
		{
			name:       "不是 115 链接",
			url:        "https://pan.baidu.com/s/1abcdefg",
			wantErrSub: "不是一条 115 分享链接",
		},
		{
			name:       "什么也没给",
			wantErrSub: "没有识别到 115 分享码",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, pwd, err := shareInput(c.url, c.code, c.pwd)
			if c.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
					t.Fatalf("应报错含 %q，实际 err=%v", c.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if code != c.wantCode || pwd != c.wantPwd {
				t.Errorf("得到 code=%q pwd=%q，想要 code=%q pwd=%q", code, pwd, c.wantCode, c.wantPwd)
			}
		})
	}
}
