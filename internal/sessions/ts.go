package sessions

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var fracRe = regexp.MustCompile(`(\.\p{Nd}{6})\p{Nd}+`)

// parseTS accepts ISO-8601 strings or epoch seconds/milliseconds.
func parseTS(v any) float64 {
	switch x := v.(type) {
	case bool: // Python bools are ints
		if x {
			return 1
		}
		return 0
	case float64:
		if x > 1e11 {
			return x / 1000.0
		}
		return x
	case string:
		s := strings.ReplaceAll(pyStrip(x), "Z", "+00:00")
		// trim sub-microsecond precision that fromisoformat rejects
		s = fracRe.ReplaceAllString(s, "$1")
		ts, ok := fromISOFormat(s)
		if !ok {
			return 0
		}
		return ts
	}
	return 0
}

// fromISOFormat ports CPython 3.14's datetime.fromisoformat(s).timestamp()
// for calendar dates (ISO week dates are not supported).
func fromISOFormat(s string) (float64, bool) {
	if utf8.RuneCountInString(s) < 7 {
		return 0, false
	}
	sep := isoDateLen(s)
	if sep < 0 {
		return 0, false
	}
	year, month, day, ok := parseISODate(s)
	if !ok {
		return 0, false
	}
	var hour, minute, second, micro, tzOff, tzMicro int
	aware := false
	if len(s) > sep {
		// the separator may be any single character
		_, sz := utf8.DecodeRuneInString(s[sep:])
		var rv int
		hour, minute, second, micro, tzOff, tzMicro, rv = parseISOTime(s[sep+sz:])
		if rv < 0 {
			return 0, false
		}
		aware = rv == 1
	}
	if year < 1 || year > 9999 || month < 1 || month > 12 || day < 1 || day > daysIn(year, month) {
		return 0, false
	}
	nextDay := false
	if hour == 24 && minute == 0 && second == 0 && micro == 0 {
		hour, nextDay = 0, true
	}
	if hour > 23 || minute > 59 || second > 59 {
		return 0, false
	}
	loc := time.Local
	if aware {
		// timezone() requires |offset| < 24h
		if total := int64(tzOff)*1e6 + int64(tzMicro); total <= -86400e6 || total >= 86400e6 {
			return 0, false
		}
		loc = time.UTC
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, loc)
	if nextDay {
		t = t.AddDate(0, 0, 1)
	}
	if aware {
		us := t.Unix()*1e6 + int64(micro) - int64(tzOff)*1e6 - int64(tzMicro)
		return float64(us) / 1e6, true
	}
	return float64(t.Unix()) + float64(micro)/1e6, true
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// isoDateLen is _find_isoformat_datetime_separator.
func isoDateLen(s string) int {
	if len(s) == 7 {
		return 7
	}
	if at(s, 4) == '-' {
		if at(s, 5) == 'W' {
			return -1
		}
		return 10
	}
	if at(s, 4) == 'W' {
		return -1
	}
	return 8
}

func digits(s string, i, n int) (int, int, bool) {
	v := 0
	for k := 0; k < n; k++ {
		c := at(s, i+k)
		if !isDigit(c) {
			return 0, i, false
		}
		v = v*10 + int(c-'0')
	}
	return v, i + n, true
}

// parseISODate is parse_isoformat_date for YYYY-MM-DD and YYYYMMDD; like the C
// code it reads the date from the start of s without checking where it ends.
func parseISODate(s string) (y, m, d int, ok bool) {
	y, p, ok := digits(s, 0, 4)
	if !ok {
		return
	}
	sep := at(s, p) == '-'
	if sep {
		p++
	}
	if m, p, ok = digits(s, p, 2); !ok {
		return
	}
	if sep {
		if at(s, p) != '-' {
			return 0, 0, 0, false
		}
		p++
	}
	d, _, ok = digits(s, p, 2)
	return y, m, d, ok
}

// parseHMS is parse_hh_mm_ss_ff over s[:end]; like the C code it may read the
// character at s[end] (a tz sign) and treats the end of s as NUL. It returns 1
// when something follows the parsed time, 0 at the end of s, <0 on error.
func parseHMS(s string, end int) (vals [3]int, micro int, rv int) {
	p := 0
	hasSep := true
	for i := 0; i < 3; i++ {
		v, np, ok := digits(s, p, 2)
		if !ok {
			return vals, 0, -3
		}
		vals[i] = v
		c := at(s, np)
		p = np + 1
		if i == 0 {
			hasSep = c == ':'
		}
		if c == '.' || c == ',' {
			if i < 2 || p >= end {
				return vals, 0, -3
			}
			break
		} else if p >= end {
			if c != 0 {
				return vals, 0, 1
			}
			return vals, 0, 0
		} else if hasSep && c == ':' {
			if i == 2 {
				return vals, 0, -4
			}
			continue
		} else if !hasSep {
			p--
		} else {
			return vals, 0, -4
		}
	}
	toParse := min(end-p, 6)
	micro, p, ok := digits(s, p, toParse)
	if !ok || toParse <= 0 {
		return vals, 0, -3
	}
	for k := toParse; k < 6; k++ {
		micro *= 10
	}
	for isDigit(at(s, p)) {
		p++
	}
	if at(s, p) != 0 {
		return vals, micro, 1
	}
	return vals, micro, 0
}

// parseISOTime is parse_isoformat_time: rv 0 naive, 1 with offset, <0 error.
func parseISOTime(s string) (h, m, sec, us, tzOff, tzUS, rv int) {
	tz := 0
	for tz < len(s) && s[tz] != '+' && s[tz] != '-' {
		tz++
	}
	if len(s) == 0 {
		tz = 1
	}
	vals, us, rv := parseHMS(s, tz)
	h, m, sec = vals[0], vals[1], vals[2]
	if rv < 0 {
		return
	}
	if tz >= len(s) {
		if rv == 1 {
			rv = -5
		}
		return
	}
	sign := 1
	if s[tz] == '-' {
		sign = -1
	}
	rest := s[tz+1:]
	tvals, tmicro, trv := parseHMS(rest, len(rest))
	if trv != 0 {
		return h, m, sec, us, 0, 0, -5
	}
	tzOff = sign * (tvals[0]*3600 + tvals[1]*60 + tvals[2])
	return h, m, sec, us, tzOff, sign * tmicro, 1
}
