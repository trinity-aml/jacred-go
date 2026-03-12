package core

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type bparser struct {
	b []byte
	i int
}

func TorrentBytesToMagnet(data []byte) string {
	infoRaw, name, announces, err := parseTorrentMeta(data)
	if err != nil || len(infoRaw) == 0 {
		return ""
	}
	h := sha1.Sum(infoRaw)
	return buildTorrentMagnet(hex.EncodeToString(h[:]), name, announces)
}

func buildTorrentMagnet(hexHash, name string, announces []string) string {
	if strings.TrimSpace(hexHash) == "" {
		return ""
	}
	parts := []string{"magnet:?xt=urn:btih:" + strings.ToLower(strings.TrimSpace(hexHash))}
	if strings.TrimSpace(name) != "" {
		parts = append(parts, "dn="+url.QueryEscape(name))
	}
	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(announces))
	for _, tr := range announces {
		tr = strings.TrimSpace(tr)
		if tr == "" {
			continue
		}
		if _, ok := seen[tr]; ok {
			continue
		}
		seen[tr] = struct{}{}
		uniq = append(uniq, tr)
	}
	sort.Strings(uniq)
	for _, tr := range uniq {
		parts = append(parts, "tr="+url.QueryEscape(tr))
	}
	return strings.Join(parts, "&")
}

func parseTorrentMeta(data []byte) ([]byte, string, []string, error) {
	p := &bparser{b: data}
	if len(p.b) == 0 || p.peek() != 'd' {
		return nil, "", nil, errors.New("torrent root is not dict")
	}
	p.i++
	var infoRaw []byte
	var announces []string
	for p.i < len(p.b) && p.peek() != 'e' {
		key, err := p.parseBytes()
		if err != nil {
			return nil, "", nil, err
		}
		switch string(key) {
		case "announce":
			v, err := p.parseBytes()
			if err != nil {
				return nil, "", nil, err
			}
			announces = append(announces, string(v))
		case "announce-list":
			vals, err := p.parseAnnounceList()
			if err != nil {
				return nil, "", nil, err
			}
			announces = append(announces, vals...)
		case "info":
			start := p.i
			if err := p.skipValue(); err != nil {
				return nil, "", nil, err
			}
			infoRaw = append([]byte(nil), p.b[start:p.i]...)
		default:
			if err := p.skipValue(); err != nil {
				return nil, "", nil, err
			}
		}
	}
	if p.i >= len(p.b) || p.peek() != 'e' {
		return nil, "", nil, errors.New("unterminated root dict")
	}
	p.i++
	return infoRaw, torrentNameFromInfo(infoRaw), announces, nil
}

func torrentNameFromInfo(infoRaw []byte) string {
	if len(infoRaw) == 0 {
		return ""
	}
	p := &bparser{b: infoRaw}
	if p.peek() != 'd' {
		return ""
	}
	p.i++
	for p.i < len(p.b) && p.peek() != 'e' {
		key, err := p.parseBytes()
		if err != nil {
			return ""
		}
		if string(key) == "name" {
			v, err := p.parseBytes()
			if err != nil {
				return ""
			}
			return string(v)
		}
		if err := p.skipValue(); err != nil {
			return ""
		}
	}
	return ""
}

func (p *bparser) parseAnnounceList() ([]string, error) {
	if p.peek() != 'l' {
		return nil, errors.New("announce-list not a list")
	}
	p.i++
	var out []string
	for p.i < len(p.b) && p.peek() != 'e' {
		if p.peek() == 'l' {
			p.i++
			for p.i < len(p.b) && p.peek() != 'e' {
				v, err := p.parseBytes()
				if err != nil {
					return nil, err
				}
				out = append(out, string(v))
			}
			if p.i >= len(p.b) || p.peek() != 'e' {
				return nil, errors.New("unterminated nested announce-list")
			}
			p.i++
			continue
		}
		v, err := p.parseBytes()
		if err != nil {
			return nil, err
		}
		out = append(out, string(v))
	}
	if p.i >= len(p.b) || p.peek() != 'e' {
		return nil, errors.New("unterminated announce-list")
	}
	p.i++
	return out, nil
}

func (p *bparser) skipValue() error {
	if p.i >= len(p.b) {
		return errors.New("unexpected eof")
	}
	switch ch := p.peek(); {
	case ch >= '0' && ch <= '9':
		_, err := p.parseBytes()
		return err
	case ch == 'i':
		p.i++
		for p.i < len(p.b) && p.b[p.i] != 'e' {
			p.i++
		}
		if p.i >= len(p.b) {
			return errors.New("unterminated int")
		}
		p.i++
		return nil
	case ch == 'l':
		p.i++
		for p.i < len(p.b) && p.peek() != 'e' {
			if err := p.skipValue(); err != nil {
				return err
			}
		}
		if p.i >= len(p.b) {
			return errors.New("unterminated list")
		}
		p.i++
		return nil
	case ch == 'd':
		p.i++
		for p.i < len(p.b) && p.peek() != 'e' {
			if _, err := p.parseBytes(); err != nil {
				return err
			}
			if err := p.skipValue(); err != nil {
				return err
			}
		}
		if p.i >= len(p.b) {
			return errors.New("unterminated dict")
		}
		p.i++
		return nil
	default:
		return errors.New("invalid bencode token")
	}
}

func (p *bparser) parseBytes() ([]byte, error) {
	start := p.i
	for p.i < len(p.b) && p.b[p.i] != ':' {
		if p.b[p.i] < '0' || p.b[p.i] > '9' {
			return nil, errors.New("invalid byte string length")
		}
		p.i++
	}
	if p.i >= len(p.b) || p.b[p.i] != ':' {
		return nil, errors.New("byte string missing colon")
	}
	n, err := strconv.Atoi(string(p.b[start:p.i]))
	if err != nil || n < 0 {
		return nil, errors.New("bad byte string length")
	}
	p.i++
	if p.i+n > len(p.b) {
		return nil, errors.New("byte string overflow")
	}
	v := p.b[p.i : p.i+n]
	p.i += n
	return v, nil
}

func (p *bparser) peek() byte { return p.b[p.i] }
