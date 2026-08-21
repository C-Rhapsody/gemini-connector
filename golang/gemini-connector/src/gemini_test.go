package main

import (
	"strings"
	"testing"
)

func TestFilePathPattern(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string // expected matches, nil if none
	}{
		{
			name:  "windows absolute path",
			input: "이미지를 C:\\Users\\bitflow\\.gemini-bot\\out\\chart.png 로 저장했습니다.",
			want:  []string{"C:\\Users\\bitflow\\.gemini-bot\\out\\chart.png"},
		},
		{
			name:  "relative path",
			input: "보고서를 output/report.pdf 에 저장했습니다.",
			want:  []string{"output/report.pdf"},
		},
		{
			name:  "bare filename",
			input: "결과는 result.csv 파일입니다.",
			want:  []string{"result.csv"},
		},
		{
			name:  "trailing punctuation excluded",
			input: "차트는 (final_v2.jpg) 여기 있습니다.",
			want:  []string{"final_v2.jpg"},
		},
		{
			name:  "source code file never matched",
			input: "golang/gemini-connector/src/main.go 를 수정했습니다.",
			want:  nil,
		},
		{
			name:  "markdown source never matched",
			input: "README.md 를 참고하세요.",
			want:  nil,
		},
		{
			name:  "absolute path to go file never matched",
			input: "C:\\proj\\src\\app.go 와 config.json 을 변경했습니다.",
			want:  nil,
		},
		{
			name:  "plain directory path without extension never matched",
			input: "C:\\Users\\bitflow\\.gemini-bot 폴더를 확인했습니다.",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filePathPattern.FindAllString(tt.input, -1)
			if len(got) != len(tt.want) {
				t.Fatalf("matches = %q, want %q", got, tt.want)
			}
			for i := range got {
				if !strings.EqualFold(got[i], tt.want[i]) {
					t.Fatalf("matches = %q, want %q", got, tt.want)
				}
			}
		})
	}
}
