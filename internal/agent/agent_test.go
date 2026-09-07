package agent

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestRenderPlistEscapesPathsAndStaysWellFormed(t *testing.T) {
	// An install path with a shell/XML metacharacter must not corrupt the plist.
	exe := "/Users/a & b/Go Tools/wgh"
	out := renderPlist(exe, "/tmp/watchgh & co/poller.log")

	if strings.Contains(out, "a & b") {
		t.Errorf("raw '&' leaked into the plist:\n%s", out)
	}
	if !strings.Contains(out, "a &amp; b") {
		t.Errorf("exe path was not XML-escaped:\n%s", out)
	}
	// The whole document must parse as XML.
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Fatalf("rendered plist is not well-formed XML: %v", err)
	}
}
