package schedule

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NLCandidate is one possible interpretation of a natural-language schedule.
// Callers show all candidates when there are several (e.g. "每天9点和18点")
// and never silently pick one.
type NLCandidate struct {
	Cron  string `json:"cron"`
	Label string `json:"label"`
	Name  string `json:"name,omitempty"`
}

// ParseNatural turns a Chinese natural-language description into cron
// candidates. It is deliberately rule-based: cc-webui's server has no model
// client of its own (it spawns CLIs), and a cron expression must be exact, so
// the fast deterministic parser is the primary path. Anything it cannot pin
// down returns an error telling the user what is missing; multiple concrete
// times return multiple candidates instead of guessing.
//
// Supported shapes (with optional 上午/下午/晚上/… and 点/点半/时/HH:mm):
//
//	每天9点… / 每日 09:00 …
//	每个工作日9:00… / 周一至周五 9点…
//	每周一9点… / 每周一和周三 18:30… / 每周日…
//	每月5号10点…
//	每小时… / 每2小时… / 每小时第15分钟…
//	每15分钟… / 每分钟…
func ParseNatural(text string) ([]NLCandidate, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("描述不能为空")
	}
	freqs, err := nlFrequencies(text)
	if err != nil {
		return nil, err
	}
	times := nlTimes(text)

	// Frequency + time combinations.
	var candidates []NLCandidate
	for _, f := range freqs {
		switch f.kind {
		case "minute":
			cron := "* * * * *"
			if f.n > 1 {
				cron = fmt.Sprintf("*/%d * * * *", f.n)
			}
			candidates = append(candidates, NLCandidate{
				Cron: cron, Label: f.label(), Name: nlSuggestName(text, [][2]int{f.span}),
			})
		case "hour":
			minute := 0
			nameSpans := [][2]int{f.span}
			if m, msp, ok := nlMinuteOfHour(text); ok {
				minute = m
				nameSpans = append(nameSpans, msp)
			}
			cron := fmt.Sprintf("%d * * * *", minute)
			if f.n > 1 {
				cron = fmt.Sprintf("%d */%d * * *", minute, f.n)
			}
			candidates = append(candidates, NLCandidate{
				Cron: cron, Label: f.label(minute), Name: nlSuggestName(text, nameSpans),
			})
		default:
			if len(times) == 0 {
				return nil, fmt.Errorf("没识别出具体时间，请补充如 9:00、上午9点、晚上8点半")
			}
			nameSpans := [][2]int{f.span}
			for _, t := range times {
				nameSpans = append(nameSpans, t.span)
			}
			for _, t := range times {
				candidates = append(candidates, NLCandidate{
					Cron:  f.cron(t.hour, t.minute),
					Label: f.label(t.hour, t.minute),
					Name:  nlSuggestName(text, nameSpans),
				})
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("没识别出可执行的排期")
	}
	// Validate every candidate with the real cron parser; drop impossible ones.
	out := candidates[:0]
	for _, c := range candidates {
		if _, err := ParseCron(c.Cron); err != nil {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("解析结果不是合法的 cron，请改用 cron 模式")
	}
	return out, nil
}

type nlFreq struct {
	kind  string // minute | hour | daily | weekdays | weekly | monthly
	n     int    // interval for minute/hour
	dows  []int
	dom   int
	span  [2]int
	spans [][2]int
}

func (f nlFreq) cron(hour, minute int) string {
	switch f.kind {
	case "daily":
		return fmt.Sprintf("%d %d * * *", minute, hour)
	case "weekdays":
		return fmt.Sprintf("%d %d * * 1-5", minute, hour)
	case "weekly":
		parts := make([]string, len(f.dows))
		for i, d := range f.dows {
			parts[i] = strconv.Itoa(d)
		}
		return fmt.Sprintf("%d %d * * %s", minute, hour, strings.Join(parts, ","))
	case "monthly":
		return fmt.Sprintf("%d %d %d * *", minute, hour, f.dom)
	}
	return ""
}

func (f nlFreq) label(args ...int) string {
	switch f.kind {
	case "minute":
		if f.n <= 1 {
			return "每分钟"
		}
		return fmt.Sprintf("每 %d 分钟", f.n)
	case "hour":
		minute := 0
		if len(args) > 0 {
			minute = args[0]
		}
		base := "每小时"
		if f.n > 1 {
			base = fmt.Sprintf("每 %d 小时", f.n)
		}
		if minute > 0 {
			return fmt.Sprintf("%s第 %d 分钟", base, minute)
		}
		return base
	case "daily":
		return fmt.Sprintf("每天 %02d:%02d", args[0], args[1])
	case "weekdays":
		return fmt.Sprintf("工作日 %02d:%02d", args[0], args[1])
	case "weekly":
		names := make([]string, len(f.dows))
		for i, d := range f.dows {
			names[i] = nlDowName(d)
		}
		return fmt.Sprintf("每%s %02d:%02d", strings.Join(names, "、"), args[0], args[1])
	case "monthly":
		return fmt.Sprintf("每月 %d 号 %02d:%02d", f.dom, args[0], args[1])
	}
	return ""
}

var (
	nlNum    = `[0-9]{1,2}|[零一二两三四五六七八九十]{1,3}`
	nlMinRe  = regexp.MustCompile(`每\s*(` + nlNum + `)\s*分钟`)
	nlMinOne = regexp.MustCompile(`每分钟`)
	nlHourRe = regexp.MustCompile(`每\s*(` + nlNum + `)?\s*个?\s*小时`)
	nlDaily  = regexp.MustCompile(`每(?:天|日|天天)|每晚|每天每`)
	nlWeekd  = regexp.MustCompile(`工作日|周一至周五|周一到周五|每个工作日`)
	nlWeekly = regexp.MustCompile(`每\s*(?:周|星期|礼拜)`)
	// A day-of-week list anchored at the start of the text after "每周"; digits
	// must carry a 周/星期/礼拜 prefix so the "18" in "每周一 18:30" is not read
	// as two weekdays.
	nlDayList = regexp.MustCompile(`^\s*(?:(?:周|星期|礼拜)\s*([一二三四五六日天0-7])|([一二三四五六日天]))(?:\s*[、,，和及]\s*(?:(?:周|星期|礼拜)\s*([一二三四五六日天0-7])|([一二三四五六日天])))*`)
	nlDowTok  = regexp.MustCompile(`[一二三四五六日天0-7]`)
	nlMonth   = regexp.MustCompile(`每\s*(?:个)?\s*月\s*(` + nlNum + `)\s*[号日]`)
	nlTimeRe  = regexp.MustCompile(`(?:(凌晨|早上|早晨|上午|中午|下午|傍晚|晚上|夜里|晚间)\s*)?(` + nlNum + `)\s*(?:[:：]\s*([0-9]{1,2})|点\s*(半|[0-9]{1,2}\s*分?)?|时\s*(半|[0-9]{1,2}\s*分?)?)`)
	nlMinOfH  = regexp.MustCompile(`第\s*([0-9]{1,2})\s*分钟`)
)

func nlFrequencies(text string) ([]nlFreq, error) {
	var out []nlFreq
	seen := map[string]bool{}
	add := func(f nlFreq) {
		if !seen[f.kind] {
			seen[f.kind] = true
			out = append(out, f)
		}
	}
	if loc := nlMinRe.FindStringSubmatchIndex(text); loc != nil {
		n := 1
		if v, ok := nlNumValue(text[loc[2]:loc[3]]); ok {
			n = v
		}
		if n < 1 || n > 59 {
			return nil, fmt.Errorf("分钟间隔超出范围（1-59）：%s", text[loc[2]:loc[3]])
		}
		add(nlFreq{kind: "minute", n: n, span: [2]int{loc[0], loc[1]}})
	} else if loc := nlMinOne.FindStringIndex(text); loc != nil {
		add(nlFreq{kind: "minute", n: 1, span: [2]int{loc[0], loc[1]}})
	}
	if loc := nlHourRe.FindStringSubmatchIndex(text); loc != nil {
		n := 1
		if loc[2] >= 0 {
			if v, ok := nlNumValue(text[loc[2]:loc[3]]); ok {
				n = v
			}
		}
		if n < 1 || n > 23 {
			return nil, fmt.Errorf("小时间隔超出范围（1-23）：%s", text[loc[2]:loc[3]])
		}
		add(nlFreq{kind: "hour", n: n, span: [2]int{loc[0], loc[1]}})
	}
	if loc := nlMonth.FindStringSubmatchIndex(text); loc != nil {
		dom, _ := nlNumValue(text[loc[2]:loc[3]])
		if dom < 1 || dom > 31 {
			return nil, fmt.Errorf("每月日期超出范围（1-31）：%s", text[loc[2]:loc[3]])
		}
		add(nlFreq{kind: "monthly", dom: dom, span: [2]int{loc[0], loc[1]}})
	}
	if nlWeekd.MatchString(text) {
		loc := nlWeekd.FindStringIndex(text)
		add(nlFreq{kind: "weekdays", span: [2]int{loc[0], loc[1]}})
	}
	if loc := nlWeekly.FindStringIndex(text); loc != nil {
		seg := nlDayList.FindString(text[loc[1]:])
		var dows []int
		for _, tok := range nlDowTok.FindAllString(seg, -1) {
			if d, ok := nlDowValue(tok); ok && !containsInt(dows, d) {
				dows = append(dows, d)
			}
		}
		if len(dows) == 0 {
			return nil, fmt.Errorf("每周后面没识别出星期几（如 每周一、工作日）")
		}
		add(nlFreq{kind: "weekly", dows: dows, span: [2]int{loc[0], loc[1] + len(seg)}})
	}
	if nlDaily.MatchString(text) {
		loc := nlDaily.FindStringIndex(text)
		add(nlFreq{kind: "daily", span: [2]int{loc[0], loc[1]}})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没识别出频率。支持：每天 / 工作日 / 每周X / 每月X号 / 每小时 / 每N分钟")
	}
	if len(out) > 1 {
		names := make([]string, len(out))
		for i, f := range out {
			names[i] = f.label(0, 0)
		}
		return nil, fmt.Errorf("同时识别到多个频率（%s），请只保留一个，或改用 cron 模式", strings.Join(names, " / "))
	}
	return out, nil
}

type nlTime struct {
	hour, minute int
	span         [2]int
}

func nlTimes(text string) []nlTime {
	var out []nlTime
	for _, loc := range nlTimeRe.FindAllStringSubmatchIndex(text, -1) {
		hour, ok := nlNumValue(text[loc[4]:loc[5]])
		if !ok {
			continue
		}
		period := ""
		if loc[2] >= 0 {
			period = text[loc[2]:loc[3]]
		}
		minute := 0
		if loc[6] >= 0 { // HH:MM
			minute, _ = strconv.Atoi(text[loc[6]:loc[7]])
		} else if loc[8] >= 0 { // 点半 / 点30分
			v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[loc[8]:loc[9]]), "分"))
			if v == "半" {
				minute = 30
			} else if v != "" {
				minute, _ = strconv.Atoi(v)
			}
		} else if loc[10] >= 0 { // 时半 / 时30分
			v := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[loc[10]:loc[11]]), "分"))
			if v == "半" {
				minute = 30
			} else if v != "" {
				minute, _ = strconv.Atoi(v)
			}
		}
		hour = nlAdjustHour(hour, period, text)
		if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			continue
		}
		out = append(out, nlTime{hour: hour, minute: minute, span: [2]int{loc[0], loc[1]}})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// nlAdjustHour applies the period prefix and "每晚"/"每天早上" style wording.
func nlAdjustHour(h int, period, text string) int {
	if period == "" {
		if strings.Contains(text, "每晚") {
			period = "晚上"
		} else if strings.Contains(text, "每天早上") || strings.Contains(text, "每日早") {
			period = "早上"
		}
	}
	switch period {
	case "下午", "傍晚", "晚上", "夜里", "晚间":
		if h < 12 {
			h += 12
		}
	case "中午":
		if h < 11 {
			h += 12
		}
	case "凌晨":
		if h == 12 {
			h = 0
		}
	}
	return h
}

func nlMinuteOfHour(text string) (int, [2]int, bool) {
	loc := nlMinOfH.FindStringSubmatchIndex(text)
	if loc == nil {
		return 0, [2]int{}, false
	}
	n, err := strconv.Atoi(text[loc[2]:loc[3]])
	if err != nil || n < 0 || n > 59 {
		return 0, [2]int{}, false
	}
	return n, [2]int{loc[0], loc[1]}, true
}

func nlSuggestName(text string, spans [][2]int) string {
	b := []byte(text)
	for _, sp := range spans {
		if sp[0] < 0 || sp[1] > len(b) || sp[0] >= sp[1] {
			continue
		}
		for i := sp[0]; i < sp[1]; i++ {
			b[i] = ' '
		}
	}
	s := strings.TrimSpace(string(b))
	s = strings.TrimLeft(s, " ，,。.;；:：!！?？、")
	// Removing several spans can leave the connective between them at the
	// front ("每天9点和18点巡检" -> "和 巡检"); it is not part of the name.
	for {
		trimmed := strings.TrimLeft(s, " 和及与、,，")
		if trimmed == s {
			break
		}
		s = trimmed
	}
	s = strings.Join(strings.Fields(s), " ")
	for _, sep := range []string{"，", ",", "。", "；", ";", "！", "!", "？", "?", "\n"} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	for _, p := range []string{"请帮我", "请", "帮我", "进行", "做一下", "做"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
		}
	}
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > 40 {
		r = r[:40]
	}
	return string(r)
}

// nlNumValue parses Arabic or simple Chinese numerals (0-99) used by the
// patterns above.
func nlNumValue(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	digits := map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	r := []rune(s)
	if len(r) == 1 {
		if d, ok := digits[r[0]]; ok {
			return d, true
		}
		if r[0] == '十' {
			return 10, true
		}
		return 0, false
	}
	// 十X / X十 / X十Y
	if r[0] == '十' && len(r) == 2 {
		if d, ok := digits[r[1]]; ok {
			return 10 + d, true
		}
	}
	if len(r) == 2 && r[1] == '十' {
		if d, ok := digits[r[0]]; ok {
			return d * 10, true
		}
	}
	if len(r) == 3 && r[1] == '十' {
		a, ok1 := digits[r[0]]
		b, ok2 := digits[r[2]]
		if ok1 && ok2 {
			return a*10 + b, true
		}
	}
	return 0, false
}

func nlDowValue(tok string) (int, bool) {
	switch tok {
	case "日", "天", "0", "7":
		return 0, true
	}
	if n, err := strconv.Atoi(tok); err == nil && n >= 0 && n <= 6 {
		return n, true
	}
	m := map[string]int{"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6}
	if d, ok := m[tok]; ok {
		return d, true
	}
	return 0, false
}

func nlDowName(d int) string {
	names := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	if d >= 0 && d < len(names) {
		return names[d]
	}
	return "周?"
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
