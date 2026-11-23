package schedule

import (
	"strings"
	"testing"
)

func TestParseNaturalBasic(t *testing.T) {
	cases := []struct {
		text string
		cron string
		name string
	}{
		{"每天9点进行看板巡检，检查常驻worker和运行中卡片是否异常", "0 9 * * *", "看板巡检"},
		{"每天 09:00 汇总昨天工作", "0 9 * * *", "汇总昨天工作"},
		{"每日上午10点半提醒我喝水", "30 10 * * *", "提醒我喝水"},
		{"每天晚上8点跑回归", "0 20 * * *", "跑回归"},
		{"每天早上9点晨报", "0 9 * * *", "晨报"},
		{"每个工作日 18:30 检查构建", "30 18 * * 1-5", "检查构建"},
		{"周一至周五 9点 站会", "0 9 * * 1-5", "站会"},
		{"每周一 9点 周会", "0 9 * * 1", "周会"},
		{"每周一和周三 18:30 跑回归", "30 18 * * 1,3", "跑回归"},
		{"每周日 9点 清理缓存", "0 9 * * 0", "清理缓存"},
		{"每月5号10点 对账", "0 10 5 * *", "对账"},
		{"每月31号 9点 月末结算", "0 9 31 * *", "月末结算"},
		{"每小时 巡检", "0 * * * *", "巡检"},
		{"每2小时第15分钟 巡检", "15 */2 * * *", "巡检"},
		{"每15分钟 检查一次", "*/15 * * * *", "检查一次"},
		{"每分钟 采样", "* * * * *", "采样"},
		{"每天九点 晨会", "0 9 * * *", "晨会"},
		{"每天下午三点半 复盘", "30 15 * * *", "复盘"},
	}
	for _, tc := range cases {
		cands, err := ParseNatural(tc.text)
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
			continue
		}
		if len(cands) != 1 {
			t.Errorf("%q: candidates = %d, want 1 (%+v)", tc.text, len(cands), cands)
			continue
		}
		if cands[0].Cron != tc.cron {
			t.Errorf("%q: cron = %q, want %q", tc.text, cands[0].Cron, tc.cron)
		}
		if cands[0].Name != tc.name {
			t.Errorf("%q: name = %q, want %q", tc.text, cands[0].Name, tc.name)
		}
		if cands[0].Label == "" {
			t.Errorf("%q: empty label", tc.text)
		}
	}
}

func TestParseNaturalAmbiguousTimes(t *testing.T) {
	cands, err := ParseNatural("每天9点和18点巡检")
	if err != nil {
		t.Fatalf("ParseNatural: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v, want 2 (never guess between two times)", cands)
	}
	if cands[0].Cron != "0 9 * * *" || cands[1].Cron != "0 18 * * *" {
		t.Fatalf("candidates = %+v", cands)
	}
	for _, c := range cands {
		if c.Name != "巡检" {
			t.Fatalf("name = %q", c.Name)
		}
	}
}

func TestParseNaturalMissingInfo(t *testing.T) {
	for _, text := range []string{
		"",
		"9点巡检",       // no frequency
		"每周一巡检",      // no time
		"每天巡检",       // no time
		"每月5号巡检",     // no time
		"每周",         // no weekday
		"每天25点巡检",    // invalid hour
		"每90分钟巡检",    // invalid interval
		"每天9点，每周一9点", // two frequencies
	} {
		if cands, err := ParseNatural(text); err == nil {
			t.Errorf("%q: expected error, got %+v", text, cands)
		}
	}
	if _, err := ParseNatural("每天和工作日9点巡检"); err == nil || !strings.Contains(err.Error(), "多个频率") {
		t.Fatalf("ambiguous frequency error = %v", err)
	}
}

func TestParseNaturalCronIsValid(t *testing.T) {
	for _, text := range []string{"每天9点巡检", "每2小时巡检", "每月31号9点巡检", "每周一和周三18:30巡检"} {
		cands, err := ParseNatural(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for _, c := range cands {
			cron, err := ParseCron(c.Cron)
			if err != nil {
				t.Fatalf("%q -> %q: %v", text, c.Cron, err)
			}
			if _, ok := cron.Next(at(2024, 1, 1, 0, 0)); !ok {
				t.Fatalf("%q -> %q never fires", text, c.Cron)
			}
		}
	}
}
