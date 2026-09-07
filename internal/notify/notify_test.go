package notify

import (
	"strings"
	"testing"
)

func TestOSAScriptEscapesQuotesAndBackslashes(t *testing.T) {
	// A title trying to break out of the string literal must stay contained.
	got := osaScript(`evil" & do shell script "rm -rf ~`, "line1\nline2\\end")

	if !strings.Contains(got, `\"`) {
		t.Errorf("double quote not escaped: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("newline not folded out of the literal: %q", got)
	}
	if !strings.Contains(got, `\\end`) {
		t.Errorf("backslash not escaped: %q", got)
	}
	// Every double quote in the output must be either the escaped \" form or one
	// of the four literal delimiters — no bare quote can terminate a literal early.
	delimiters := strings.Count(got, `"`) - strings.Count(got, `\"`)
	if delimiters != 4 {
		t.Errorf("want 4 literal-delimiter quotes, got %d: %q", delimiters, got)
	}
}
