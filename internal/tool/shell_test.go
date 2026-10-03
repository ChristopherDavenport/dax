package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBashSubjects(t *testing.T) {
	type subj struct{ tool, text string }
	tests := []struct {
		cmd     string
		want    []subj
		wantErr string
	}{
		{"git status", []subj{{"", "git status"}}, ""},
		{"git status && rm -rf /", []subj{{"", "git status"}, {"", "rm -rf /"}}, ""},
		{"a; b || c | d & e\nf", []subj{{"", "a"}, {"", "b"}, {"", "c"}, {"", "d"}, {"", "e"}, {"", "f"}}, ""},
		{`echo "a && b" 'c ; d'`, []subj{{"", `echo "a && b" 'c ; d'`}}, ""},
		{"go test ./... 2>&1", []subj{{"", "go test ./... 2>&1"}}, ""},
		{"go test > /dev/null 2>&1", []subj{{"", "go test  2>&1"}}, ""},
		{"git log > out.txt", []subj{{"", "git log"}, {"write", "out.txt"}}, ""},
		{"git log >> ~/.bashrc", []subj{{"", "git log"}, {"write", "~/.bashrc"}}, ""},
		{`git log >"a b"`, []subj{{"", "git log"}, {"write", "a b"}}, ""},
		{"git log &> out", []subj{{"", "git log"}, {"write", "out"}}, ""},
		{"git log > $F", []subj{{"", "git log"}, {"write", "$F"}, {"", "$(...) git log > $F"}}, ""},
		{"git log $(rm -rf x)", []subj{{"", "git log $(rm -rf x)"}, {"", "$(...) git log $(rm -rf x)"}}, ""},
		{"git log `rm x`", []subj{{"", "git log `rm x`"}, {"", "$(...) git log `rm x`"}}, ""},
		{`echo "$(rm x)"`, []subj{{"", `echo "$(rm x)"`}, {"", `$(...) echo "$(rm x)"`}}, ""},
		{`echo '$(safe)'`, []subj{{"", `echo '$(safe)'`}}, ""},
		{"diff <(a) <(b)", []subj{{"", "diff <(a) <(b)"}, {"", "$(...) diff <(a) <(b)"}}, ""},
		{"echo 'unterminated", nil, "unterminated quote"},
		{"   ", nil, "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			args, _ := json.Marshal(map[string]string{"command": tc.cmd})
			got, err := BashSubjects(args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var have []subj
			for _, s := range got {
				var m map[string]string
				json.Unmarshal(s.Args, &m)
				field := "command"
				if s.Tool == "write" {
					field = "path"
				}
				if m[field] != s.Text {
					t.Errorf("args %s do not carry text %q", s.Args, s.Text)
				}
				have = append(have, subj{s.Tool, s.Text})
			}
			if !reflect.DeepEqual(have, tc.want) {
				t.Fatalf("got  %q\nwant %q", have, tc.want)
			}
		})
	}
}
