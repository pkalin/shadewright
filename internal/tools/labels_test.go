package tools

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
)

// A tool result and the context use the same text for a log excerpt whose restart is uncertain (#951).
func TestLogInstanceUncertainLabelMatchesContext(t *testing.T) {
	if string(LabelLogInstanceUncertain) != string(incidentcontext.LabelLogInstanceUncertain) {
		t.Fatalf("tool label %q, context label %q", LabelLogInstanceUncertain, incidentcontext.LabelLogInstanceUncertain)
	}
	r := Result{Tool: "get_logs", Status: StatusOK, Labels: []Label{LabelLogInstanceUncertain}, Text: "line\n"}
	want := `labels="log excerpt from an uncertain instance of this container (a restart or a re-created pod)">`
	if c := r.Content(); !strings.Contains(c, want) {
		t.Fatalf("content lacks %s:\n%s", want, c)
	}
}
