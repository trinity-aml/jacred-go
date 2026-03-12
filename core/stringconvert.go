package core

import (
	"regexp"
	"strings"
)

var searchNameRe = regexp.MustCompile(`[^a-zA-Zа-яА-Я0-9Ёё]+`)

func SearchName(val string) string {
	if strings.TrimSpace(val) == "" {
		return ""
	}
	v := strings.ToLower(val)
	v = searchNameRe.ReplaceAllString(v, "")
	v = strings.ReplaceAll(v, "ё", "е")
	v = strings.ReplaceAll(v, "щ", "ш")
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}
