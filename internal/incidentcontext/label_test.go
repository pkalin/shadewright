package incidentcontext

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// The label of a log excerpt the source cannot tie to one restart names the container and the restart, and
// never the pod: the pod and the container are certain, only which restart is not (#951).
func TestLogInstanceUncertainLabelNamesContainerAndRestart(t *testing.T) {
	const want = "log excerpt from this container; which restart it comes from is uncertain"
	l := string(LabelLogInstanceUncertain)
	if l != want {
		t.Fatalf("label %q, want %q", l, want)
	}
	if strings.Contains(l, "pod") {
		t.Errorf("label %q names the pod, which is not in doubt", l)
	}
	if r := redact.String(l); r != l {
		t.Errorf("redaction changes the label to %q", r)
	}
	for _, s := range []string{"\"", "<", ">", "\n"} {
		if strings.Contains(l, s) {
			t.Errorf("label %q holds %q, which the block header cannot carry plainly", l, s)
		}
	}
}
