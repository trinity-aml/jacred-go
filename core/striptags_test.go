package core

import "testing"

func TestStripTagsAndCollapseSpaces(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"plain text", "plain text"},
		{"<b>Hello</b> <i>world</i>", "Hello world"},
		{"  leading\tand\ntrailing  ", "leading and trailing"},
		{"NBSP split", "NBSP split"},
		{"<a href=\"x\">link</a> text", "link text"},
		{"multi   spaces", "multi spaces"},
		{"<p><b>nested</b> <i>tags</i></p>", "nested tags"},
		// Unterminated tag — drop the tail to avoid leaking <…> into output.
		{"good <bad without close", "good"},
		{"мульти текст с\tNBSP", "мульти текст с NBSP"},
	}
	for _, c := range cases {
		got := StripTagsAndCollapseSpaces(c.in)
		if got != c.want {
			t.Errorf("input=%q\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}
