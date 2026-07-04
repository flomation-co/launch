package http

import (
	"testing"

	slackpkg "flomation.app/automate/launch/internal/slack"
)

func TestParseHITLActionID(t *testing.T) {
	cases := []struct {
		in      string
		wantReq string
		wantOpt string
		wantOK  bool
	}{
		{"hitl:req-123:yes", "req-123", "yes", true},
		{"hitl:abc-def-ghi:approve_deploy", "abc-def-ghi", "approve_deploy", true},
		{"nothitl:req:yes", "", "", false},
		{"hitl:missingoption", "", "", false},
		{"hitl:req:", "", "", false},
		{"hitl::yes", "", "", false},
	}
	for _, c := range cases {
		req, opt, ok := parseHITLActionID(c.in)
		if ok != c.wantOK || req != c.wantReq || opt != c.wantOpt {
			t.Errorf("parseHITLActionID(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, req, opt, ok, c.wantReq, c.wantOpt, c.wantOK)
		}
	}
}

func TestFirstHITLAction_FindsHITLAmongOthers(t *testing.T) {
	actions := []slackpkg.InteractionAction{
		{ActionID: "some_other_button", Value: "x"},
		{ActionID: "hitl:req-9:no", Value: "no"},
	}
	req, opt, ok := firstHITLAction(actions)
	if !ok || req != "req-9" || opt != "no" {
		t.Errorf("firstHITLAction = (%q,%q,%v), want (req-9,no,true)", req, opt, ok)
	}

	none := []slackpkg.InteractionAction{{ActionID: "plain", Value: "y"}}
	if _, _, ok := firstHITLAction(none); ok {
		t.Error("firstHITLAction should return false when no hitl action present")
	}
}
