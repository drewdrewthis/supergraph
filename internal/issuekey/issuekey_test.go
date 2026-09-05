package issuekey

import (
	"reflect"
	"testing"
)

func TestFromBranch(t *testing.T) {
	cases := []struct {
		branch string
		wantN  int
		wantOK bool
	}{
		// positives — the agreed branch forms (docs/edr/github-query.md D3).
		{"issue1/spike-core", 1, true},
		{"issue-12", 12, true},
		{"issue12-foo", 12, true},
		{"issue7", 7, true},
		// negatives — the prefix is not anchored, has no digits, or a non-boundary tail.
		{"fix/issue3", 0, false},
		{"myissue4", 0, false},
		{"issue", 0, false},
		{"issueX/1", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		gotN, gotOK := FromBranch(c.branch)
		if gotN != c.wantN || gotOK != c.wantOK {
			t.Errorf("FromBranch(%q) = (%d, %t), want (%d, %t)", c.branch, gotN, gotOK, c.wantN, c.wantOK)
		}
	}
}

func TestClosingRefs(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []int
	}{
		{"single close", "Closes #42", []int{42}},
		{"two keywords one string", "fixed #3 and resolves #4", []int{3, 4}},
		{"case insensitive", "CLOSES #7", []int{7}},
		{"bare mention no keyword", "see #9 for context", nil},
		{"cross-repo dropped", "Fixes other/repo#5", nil},
		{"mixed local and cross-repo and mention", "Closes #1 and fixes other/repo#2, mentions #3, resolves #4", []int{1, 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClosingRefs(c.text); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ClosingRefs(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}
