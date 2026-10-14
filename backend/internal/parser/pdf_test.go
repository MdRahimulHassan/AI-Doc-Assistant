package parser

import (
	"strings"
	"testing"
)

func TestLooksGlued(t *testing.T) {
	english := strings.Repeat("The zebrafish is a small freshwater fish used in laboratories. ", 20)
	glued := strings.Repeat("Thezebrafishisasmallfreshwaterfishusedinlaboratories.", 20)

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"normal English", english, false},
		{"fused words", glued, true},
		{"fused words with an occasional space", strings.Repeat(glued[:200]+" ", 10), true},
		{"too short to judge", "Thezebrafishisasmallfreshwaterfish", false},
		{"Chinese without spaces is not judged", strings.Repeat("这是一个没有空格的中文句子。", 100), false},
		{"Bengali with spaces", strings.Repeat("আমি বই পড়ি ", 100), false},
		{"empty", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksGlued(tc.in); got != tc.want {
				t.Errorf("looksGlued = %v, want %v", got, tc.want)
			}
		})
	}
}