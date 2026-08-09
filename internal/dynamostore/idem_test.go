package dynamostore

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestIdemOutcome covers the four ways a condition-failed claim can resolve.
// Two of them cannot be provoked against a live DynamoDB from a test — a
// PutItem that commits and loses its acknowledgement, and a TTL that reaps the
// record inside the gap between the write and the read — which is why the
// decision is a pure function. It needs no endpoint.
func TestIdemOutcome(t *testing.T) {
	const mine = "MYCLAIM"
	rec := func(state, claim string, resp []byte) map[string]types.AttributeValue {
		item := map[string]types.AttributeValue{
			"state": attrS(state),
			"claim": attrS(claim),
		}
		if len(resp) > 0 {
			item["resp"] = attrB(resp)
		}
		return item
	}
	tests := []struct {
		name      string
		item      map[string]types.AttributeValue
		wantResp  string
		wantDone  bool
		wantFound bool
	}{
		// The record was reaped between our write and our read: in-flight, so
		// a retry claims it cleanly rather than two callers executing.
		{"reaped in the gap", nil, "", false, true},
		// Our own committed write, seen only by the SDK's retry. We hold the
		// claim — reporting in-flight here wedges the op until the TTL.
		{"our own lost acknowledgement", rec(idemPending, mine, nil), "", false, false},
		{"another caller in flight", rec(idemPending, "THEIRS", nil), "", false, true},
		{"already done", rec(idemDone, "THEIRS", []byte(`{"ok":true}`)), `{"ok":true}`, true, true},
		// Done with no recorded body must stay done, not degrade to in-flight.
		{"done with an empty response", rec(idemDone, "THEIRS", nil), "", true, true},
	}
	for _, tc := range tests {
		resp, done, found := idemOutcome(tc.item, mine)
		if string(resp) != tc.wantResp || done != tc.wantDone || found != tc.wantFound {
			t.Errorf("%s: resp=%q done=%v found=%v, want resp=%q done=%v found=%v",
				tc.name, resp, done, found, tc.wantResp, tc.wantDone, tc.wantFound)
		}
	}
}
