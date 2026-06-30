package progress

import (
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// TestClassifyHITLResponseAlwaysAnswer verifies that pure-text channels always
// get ResponseAnswer — no keyword matching. Intent is left to the agent (#92).
func TestClassifyHITLResponseAlwaysAnswer(t *testing.T) {
	cases := []struct {
		text    string
		reqKind humanrequest.RequestKind
	}{
		{"同意", humanrequest.RequestApproval},
		{"拒绝", humanrequest.RequestApproval},
		{"不要", humanrequest.RequestApproval},
		{"取消", humanrequest.RequestApproval},
		{"hello world", humanrequest.RequestFreeform},
		{"42", humanrequest.RequestApproval},
		{"", humanrequest.RequestFreeform},
		{"嗯", humanrequest.RequestApproval},
		{"我觉得可以这样做", humanrequest.RequestApproval},
	}
	for _, tc := range cases {
		kind, msg := ClassifyHITLResponse(tc.text, tc.reqKind)
		if kind != humanrequest.ResponseAnswer {
			t.Errorf("ClassifyHITLResponse(%q, %v) kind = %v, want answer (no keyword matching)", tc.text, tc.reqKind, kind)
		}
		// Empty input is padded to " " (Store requires non-empty for answer).
		if tc.text != "" && msg != tc.text {
			t.Errorf("ClassifyHITLResponse(%q, %v) msg = %q, want %q", tc.text, tc.reqKind, msg, tc.text)
		}
	}
}
