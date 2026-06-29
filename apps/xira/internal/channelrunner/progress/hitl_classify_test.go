package progress

import (
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

func TestClassifyHITLResponseFreeform(t *testing.T) {
	cases := []struct {
		text string
		want humanrequest.ResponseKind
	}{
		{"hello world", humanrequest.ResponseAnswer},
		{"42", humanrequest.ResponseAnswer},
		{"", humanrequest.ResponseAnswer}, // empty → padded to " "
	}
	for _, tc := range cases {
		kind, msg := ClassifyHITLResponse(tc.text, humanrequest.RequestFreeform)
		if kind != tc.want {
			t.Errorf("ClassifyHITLResponse(%q, freeform) kind = %v, want %v", tc.text, kind, tc.want)
		}
		if tc.text != "" && msg != tc.text {
			t.Errorf("ClassifyHITLResponse(%q, freeform) msg = %q, want %q", tc.text, msg, tc.text)
		}
	}
}

func TestClassifyHITLResponseApprovalApprove(t *testing.T) {
	for _, text := range []string{"同意", "是", "好", "ok", "OK", "yes", "Yes", "approve", "确认", "对", "可以", "y"} {
		kind, _ := ClassifyHITLResponse(text, humanrequest.RequestApproval)
		if kind != humanrequest.ResponseApprove {
			t.Errorf("ClassifyHITLResponse(%q, approval) = %v, want approve", text, kind)
		}
	}
}

func TestClassifyHITLResponseApprovalDeny(t *testing.T) {
	for _, text := range []string{"拒绝", "否", "no", "NO", "deny", "不行", "不对", "n"} {
		kind, _ := ClassifyHITLResponse(text, humanrequest.RequestApproval)
		if kind != humanrequest.ResponseDeny {
			t.Errorf("ClassifyHITLResponse(%q, approval) = %v, want deny", text, kind)
		}
	}
}

func TestClassifyHITLResponseApprovalCancel(t *testing.T) {
	for _, text := range []string{"取消", "cancel", "算了", "不要了"} {
		kind, _ := ClassifyHITLResponse(text, humanrequest.RequestApproval)
		if kind != humanrequest.ResponseCancel {
			t.Errorf("ClassifyHITLResponse(%q, approval) = %v, want cancel", text, kind)
		}
	}
}

func TestClassifyHITLResponseApprovalFallback(t *testing.T) {
	// Short unknown text (≤2 runes) → approve (most common intent)
	kind, _ := ClassifyHITLResponse("嗯", humanrequest.RequestApproval)
	if kind != humanrequest.ResponseApprove {
		t.Errorf("ClassifyHITLResponse(\"嗯\", approval) = %v, want approve (short fallback)", kind)
	}
	// Longer unknown text → answer
	kind, _ = ClassifyHITLResponse("我觉得可以这样做", humanrequest.RequestApproval)
	if kind != humanrequest.ResponseAnswer {
		t.Errorf("ClassifyHITLResponse(\"我觉得可以这样做\", approval) = %v, want answer (long fallback)", kind)
	}
}
