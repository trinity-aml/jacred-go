package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// StripTagsAndCollapseSpaces removes <…> tag spans and collapses runs of
// whitespace (including U+00A0 NBSP) to single ASCII spaces in one pass.
// Trims leading/trailing space implicitly. Combines what was previously a
// stripTags + ReplaceAll(spaceCleanupRe) + TrimSpace chain into one
// allocation per call. Pair with html.UnescapeString when input may carry
// HTML entities (UnescapeString stays separate because an entity may
// itself contain `<` after decoding, which a tag-stripper would
// misinterpret).
func StripTagsAndCollapseSpaces(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	// Start as if a space already preceded the buffer so leading whitespace
	// gets absorbed; flip to false on the first real character.
	prevSpace := true
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if inTag {
			if r == '>' {
				inTag = false
			}
			continue
		}
		if r == '<' {
			inTag = true
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ' ' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	out := b.String()
	if n := len(out); n > 0 && out[n-1] == ' ' {
		out = out[:n-1]
	}
	return out
}

// --- Compiled regex cache ---
//
// Some parsers iterate over a slice of literal pattern strings and call
// regexp.MustCompile per row. Each compile is ~10–50 µs and accumulates to
// thousands of redundant ops per page on hot paths like parseTitle. Use
// CachedRegex when patterns are static literals known at runtime — the first
// call compiles, subsequent calls return the same *regexp.Regexp under
// RWMutex. A nil result is cached for malformed patterns so we don't retry.

var (
	regexCacheMu sync.RWMutex
	regexCache   = map[string]*regexp.Regexp{}
)

// CachedRegex returns a compiled *regexp.Regexp for pattern, caching it
// across the process. Returns nil when pattern is invalid. Patterns must be
// static literals — callers passing dynamically constructed strings risk
// unbounded cache growth.
func CachedRegex(pattern string) *regexp.Regexp {
	regexCacheMu.RLock()
	if re, ok := regexCache[pattern]; ok {
		regexCacheMu.RUnlock()
		return re
	}
	regexCacheMu.RUnlock()
	regexCacheMu.Lock()
	defer regexCacheMu.Unlock()
	if re, ok := regexCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCache[pattern] = nil
		return nil
	}
	regexCache[pattern] = re
	return re
}

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
		// strconv.Atoi is ~10× faster than fmt.Sscanf and avoids escaping
		// the int destination to the heap via &i.
		i, _ := strconv.Atoi(strings.TrimSpace(n))
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
