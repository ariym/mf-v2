package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestRequestApproval(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		yes, ok bool
	}{{"\n", false, true}, {"Y\n", false, true}, {"yes\n", false, true}, {"n\n", false, false}, {"", false, false}, {"Y", false, false}, {"", true, true}, {"maybe\n", false, false}} {
		var out bytes.Buffer
		err := requestAnswer(bufio.NewReader(strings.NewReader(tc.answer)), &out, "install?", tc.yes)
		if (err == nil) != tc.ok {
			t.Errorf("answer %q --yes=%v: %v", tc.answer, tc.yes, err)
		}
		if !strings.Contains(out.String(), "Y/n") {
			t.Fatal("missing approval prompt")
		}
	}
}
