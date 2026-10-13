package parser

import "testing"

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "hello world", "hello world"},
		{"NUL bytes removed", "a\x00b", "ab"},
		{"other control characters removed", "a\x07b", "ab"},
		{"CRLF and CR become LF", "a\r\nb\rc", "a\nb\nc"},
		{"runs of spaces and tabs collapse", "  hello   world \t again  ", "hello world again"},
		{"non-breaking spaces collapse", "a\u00a0\u00a0b", "a b"},
		{"many blank lines become one", "para one\n\n\n\n\npara two", "para one\n\npara two"},
		{"single line break is kept", "line one\nline two", "line one\nline two"},
		{"spaces around lines trimmed", "line one  \n   line two", "line one\nline two"},
		{"form feed is a paragraph break", "page1\fpage2", "page1\n\npage2"},
		{"invalid UTF-8 repaired", "ab\xffcd", "ab cd"},
		{"BOM removed", "\ufeffhello", "hello"},
		{"zero-width space removed", "a\u200bb", "ab"},
		{"soft hyphen removed", "soft\u00adhyphen", "softhyphen"},
		{"joiners kept (needed for Bengali and others)", "a\u200cb", "a\u200cb"},
		{"accents unchanged", "café naïve résumé", "café naïve résumé"},
		{"Bengali unchanged", "আমি বই পড়ি", "আমি বই পড়ি"},
		{"ligatures expanded", "ﬁnding ﬂow oﬃce", "finding flow office"},
		{"only whitespace becomes empty", "  \n\t \r\n ", ""},
		{"empty stays empty", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Clean(tc.in)
			if got != tc.want {
				t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := Clean(got); again != got {
				t.Errorf("not idempotent: Clean(Clean(x)) = %q, want %q", again, got)
			}
		})
	}
}