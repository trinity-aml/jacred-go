package core

// DecodeCP1251 converts Windows-1251 bytes to UTF-8 string without external deps.
func DecodeCP1251(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		switch {
		case c < 0x80:
			r[i] = rune(c)
		case c >= 0xC0:
			r[i] = rune(0x0410) + rune(c-0xC0)
		case c == 0xA8:
			r[i] = 'Ё'
		case c == 0xB8:
			r[i] = 'ё'
		case c == 0xAA:
			r[i] = 'Є'
		case c == 0xBA:
			r[i] = 'є'
		case c == 0xAF:
			r[i] = 'Ї'
		case c == 0xBF:
			r[i] = 'ї'
		case c == 0xB2:
			r[i] = 'І'
		case c == 0xB3:
			r[i] = 'і'
		case c == 0xA5:
			r[i] = 'Ґ'
		case c == 0xB4:
			r[i] = 'ґ'
		case c == 0x96:
			r[i] = '–'
		case c == 0x97:
			r[i] = '—'
		case c == 0x91 || c == 0x92:
			r[i] = '\''
		case c == 0x93 || c == 0x94:
			r[i] = '"'
		case c == 0x85:
			r[i] = '…'
		case c == 0xA0:
			r[i] = '\u00a0'
		default:
			r[i] = rune(cp1251Supplement[c])
			if r[i] == 0 {
				r[i] = rune(c)
			}
		}
	}
	return string(r)
}

var cp1251Supplement = map[byte]rune{
	0x80: 'Ђ', 0x81: 'Ѓ', 0x82: '‚', 0x83: 'ѓ', 0x84: '„', 0x86: '†', 0x87: '‡',
	0x88: '€', 0x89: '‰', 0x8A: 'Љ', 0x8B: '‹', 0x8C: 'Њ', 0x8D: 'Ќ', 0x8E: 'Ћ', 0x8F: 'Џ',
	0x90: 'ђ', 0x95: '•', 0x99: '™', 0x9A: 'љ', 0x9B: '›', 0x9C: 'њ', 0x9D: 'ќ', 0x9E: 'ћ', 0x9F: 'џ',
	0xA1: 'Ў', 0xA2: 'ў', 0xA3: 'Ј', 0xA4: '¤', 0xA6: '¦', 0xA7: '§',
	0xA9: '©', 0xAB: '«', 0xAC: '¬', 0xAD: '\u00ad', 0xAE: '®',
	0xB0: '°', 0xB1: '±', 0xB5: 'µ', 0xB6: '¶', 0xB7: '·', 0xB9: '№', 0xBB: '»',
	0xBC: 'ј', 0xBD: 'Ѕ', 0xBE: 'ѕ',
}
