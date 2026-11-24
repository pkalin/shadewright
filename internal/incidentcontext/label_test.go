package incidentcontext

import (
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact"
)

// The label of a log excerpt the source cannot tie to one container instance names the instance as uncertain,
// with both causes (a restart, a re-created pod), and never claims the pod or the container is certain (#951).
func TestLogInstanceUncertainLabelNamesBothCauses(t *testing.T) {
	const want = "log excerpt from an uncertain instance of this container (a restart or a re-created pod)"
	l := string(LabelLogInstanceUncertain)
	if l != want {
		t.Fatalf("label %q, want %q", l, want)
	}
	for _, cause := range []string{"restart", "re-created pod", "uncertain"} {
		if !strings.Contains(l, cause) {
			t.Errorf("label %q does not name %q", l, cause)
		}
	}
	// The source also sets the mark for a pod re-created under the same name, so the lines may be another
	// pod's: a label that says they come from this container would be false then.
	if strings.Contains(l, "from this container") {
		t.Errorf("label %q claims the container instance is certain", l)
	}
	// Block headers join labels with "; ", so one label must not hold it.
	if strings.Contains(l, ";") {
		t.Errorf("label %q holds a semicolon and would read as two labels", l)
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
