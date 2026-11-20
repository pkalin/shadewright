package analysis

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/incidentcontext"
)

// The model sees the reworded label of a log excerpt whose restart is uncertain, whole, in the block's header
// (#951).
func TestContextShowsLogInstanceUncertainLabel(t *testing.T) {
	c := incidentcontext.Context{Blocks: []incidentcontext.Block{{
		Section:      incidentcontext.SectionLogs,
		Provenance:   incidentcontext.ProvenanceLogExcerpt,
		Availability: incidentcontext.Available,
		Labels:       []incidentcontext.Label{incidentcontext.LabelLogInstanceUncertain},
		Text:         "pod web-1, container web, previous instance\nline\n",
	}}}
	msg, _ := contextMessage(&c)
	want := `labels="log excerpt from this container; which restart it comes from is uncertain">`
	if !strings.Contains(msg, want) {
		t.Fatalf("context lacks %s:\n%s", want, msg)
	}
	if strings.Contains(msg, "uncertain about its instance") {
		t.Errorf("context shows the old label:\n%s", msg)
	}
}
