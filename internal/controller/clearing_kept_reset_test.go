package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
)

// A request that reuses the name of an earlier one never inherits the reset kept for it.
func TestKeptResetIsBoundToTheRequestUID(t *testing.T) {
	first := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr", UID: types.UID("a")}}
	second := first.DeepCopy()
	second.UID = types.UID("b")

	at := metav1.NewTime(time.Unix(1000, 0))
	var kept unwrittenResets
	kept.note(first, verification.Window{LastReset: &remediationv1alpha1.WindowReset{Reason: remediationv1alpha1.ResetReason("PodNotReady"), Time: at}})

	if _, ok := kept.get(first); !ok {
		t.Fatal("the request that left the reset does not get it back")
	}
	if _, ok := kept.get(second); ok {
		t.Fatal("a request with another UID got the reset of the one whose name it reuses")
	}
	if _, ok := kept.get(first); ok {
		t.Fatal("the entry stayed after a request with another UID was seen under the name")
	}
}
