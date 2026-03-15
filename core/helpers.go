package core

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- String/int/float conversion helpers ---

func AsString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func AsInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(strings.TrimSpace(n), "%d", &i)
		return i
	case nil:
		return 0
	default:
		n2, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
		return n2
	}
}

func AsInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	case nil:
		return 0
	default:
		n, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(v)), 10, 64)
		return n
	}
}

func AsFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f
	case nil:
		return 0
	default:
		return 0
	}
}

func AsStringSlice(v any) []string {
	switch arr := v.(type) {
	case []string:
		return arr
	case []any:
		out := make([]string, 0, len(arr))
		for _, it := range arr {
			out = append(out, AsString(it))
		}
		return out
	case nil:
		return nil
	default:
		return nil
	}
}

func AsIntSlice(v any) []int {
	switch x := v.(type) {
	case []any:
		out := make([]int, 0, len(x))
		for _, it := range x {
			out = append(out, AsInt(it))
		}
		return out
	case []int:
		return x
	default:
		return nil
	}
}

// --- Time parsing helpers ---

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.9999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	time.RFC3339,
}

func ParseTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		return ParseTimeString(t)
	case nil:
		return time.Time{}
	default:
		return time.Time{}
	}
}

func ParseTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// --- Collection helpers ---

func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func ContainsString(vals []string, want string) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func ContainsStringFold(vals []string, want string) bool {
	for _, v := range vals {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

func ContainsInt(vals []int, want int) bool {
	for _, v := range vals {
		if v == want {
			return true
		}
	}
	return false
}

func UniqueStrings(vals []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func SortedUniqueStrings(vals []string) []string {
	out := UniqueStrings(vals)
	sort.Strings(out)
	return out
}

func MakeStringSet(vals []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, v := range vals {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

func HasAny(hay []string, want ...string) bool {
	set := MakeStringSet(hay)
	for _, v := range want {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

func SortedIntKeys(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func SortedStringKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
