// Package schedule implements the cron parser, JSON persistence and due-task
// claiming for AgentDeck's scheduled prompts. It is a leaf package (standard
// library only) so the server can use it without pulling in runner/store.
//
// Time zone is deliberately the process-local zone (time.Local): cron fields
// are interpreted as wall-clock time on the machine running AgentDeck (CST on
// the deployment this feature was built for).
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// nextHorizon bounds the minute-by-minute search in Cron.Next. It is long
// enough to cover the rarest useful pattern (Feb 29, every 4 years) and keeps
// an impossible expression from spinning forever.
const nextHorizon = 5 * 366 * 24 * time.Hour

// Cron is a parsed 5-field cron expression: minute hour day-of-month month
// day-of-week. Supported syntax per field: *, n, a-b, */step, a-b/step, n/step
// and comma-separated lists. Day-of-week accepts 0-7 with both 0 and 7 meaning
// Sunday. When both day-of-month and day-of-week are restricted, either match
// is enough (the classic Vixie-cron rule).
type Cron struct {
	expr  string
	min   uint64
	hour  uint64
	dom   uint64
	month uint64
	dow   uint64
	// domStar/dowStar record whether the field was exactly "*". Only the
	// unrestricted form participates in the OR/AND day rule; "*/2" counts as
	// restricted, matching Vixie cron.
	domStar bool
	dowStar bool
}

// Expr returns the normalized expression (single spaces between fields).
func (c *Cron) Expr() string { return c.expr }

// String implements fmt.Stringer.
func (c *Cron) String() string { return c.expr }

type cronField struct {
	name string
	min  int
	max  int
}

var cronFields = []cronField{
	{"分钟", 0, 59},
	{"小时", 0, 23},
	{"日", 1, 31},
	{"月", 1, 12},
	{"星期", 0, 7},
}

// ParseCron parses a 5-field cron expression. The returned error is intended
// for the HTTP API and therefore written in Chinese.
func ParseCron(expr string) (*Cron, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 需要 5 个字段（分 时 日 月 周），当前 %d 个", len(fields))
	}
	c := &Cron{expr: strings.Join(fields, " ")}
	bits := make([]uint64, 5)
	for i, f := range fields {
		b, err := parseCronField(f, cronFields[i])
		if err != nil {
			return nil, err
		}
		bits[i] = b
	}
	c.min, c.hour, c.dom, c.month, c.dow = bits[0], bits[1], bits[2], bits[3], bits[4]
	c.domStar = fields[2] == "*"
	c.dowStar = fields[4] == "*"
	// 7 is an accepted alias for Sunday; fold it onto bit 0.
	if c.dow&(1<<7) != 0 {
		c.dow = (c.dow | 1) &^ (1 << 7)
	}
	return c, nil
}

func parseCronField(raw string, spec cronField) (uint64, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s字段为空", spec.name)
	}
	var bits uint64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("%s字段里有空项", spec.name)
		}
		b, err := parseCronPart(part, spec)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	return bits, nil
}

func parseCronPart(part string, spec cronField) (uint64, error) {
	step := 1
	if i := strings.IndexByte(part, '/'); i >= 0 {
		stepStr := part[i+1:]
		part = part[:i]
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%s字段的步长 %q 无效", spec.name, stepStr)
		}
		step = n
	}
	if part == "" {
		return 0, fmt.Errorf("%s字段的步长缺少起始值", spec.name)
	}

	var lo, hi int
	switch {
	case part == "*":
		lo, hi = spec.min, spec.max
	case strings.Contains(part, "-"):
		segs := strings.SplitN(part, "-", 2)
		a, err1 := strconv.Atoi(segs[0])
		b, err2 := strconv.Atoi(segs[1])
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("%s字段的范围 %q 无效", spec.name, part)
		}
		lo, hi = a, b
		if lo > hi {
			return 0, fmt.Errorf("%s字段的范围 %q 起点大于终点", spec.name, part)
		}
	default:
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("%s字段的值 %q 不是数字", spec.name, part)
		}
		lo, hi = n, n
		// `5/15` means "from 5 to the field max, every 15".
		if step > 1 {
			hi = spec.max
		}
	}
	if lo < spec.min || hi > spec.max {
		return 0, fmt.Errorf("%s字段的值超出范围 %d-%d：%q", spec.name, spec.min, spec.max, part)
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

// Next returns the first matching minute strictly after t, in t's location.
// ok is false when no match exists within five years (e.g. "0 0 30 2 *").
func (c *Cron) Next(t time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	cur := t.Truncate(time.Minute).Add(time.Minute)
	limit := cur.Add(nextHorizon)
	for cur.Before(limit) {
		if c.matches(cur) {
			return cur, true
		}
		cur = cur.Add(time.Minute)
	}
	return time.Time{}, false
}

func (c *Cron) matches(t time.Time) bool {
	if c.min&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if c.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if c.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}
	domOK := c.dom&(1<<uint(t.Day())) != 0
	dowOK := c.dow&(1<<uint(int(t.Weekday()))) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dowOK
	case c.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}
