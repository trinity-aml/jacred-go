package core

import (
	"crypto/md5"
	"encoding/hex"
)

func MD5(text string) string {
	h := md5.Sum([]byte(text))
	return hex.EncodeToString(h[:])
}

func NameToHash(name, original string) string {
	searchName := SearchName(name)
	searchOriginal := SearchName(original)
	if searchName == "" {
		searchName = searchOriginal
	}
	if searchOriginal == "" {
		searchOriginal = searchName
	}
	return searchName + ":" + searchOriginal
}
