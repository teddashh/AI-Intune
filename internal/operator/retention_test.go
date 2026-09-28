package operator

import (
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestRetentionTransportRejectionOwnsItsAuditShape(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/retention.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := New(st)
	request := RetentionTransportRejectionRequest{
		Code: RetentionTransportRejectionCoordinatesInvalid, IdempotencyKey: "web-retention-malformed",
		Actor: Actor{
			SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "admin", AuthDecision: "authorized", SourceKind: SourceKindWeb,
		},
	}
	if err := service.RecordRetentionTransportRejection(request); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("transport audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	wantDetail := store.OperatorTransportRejectionPrefix + string(request.Code) +
		": canonical request digest 無法取得"
	if entry.Action != store.AuditRetentionPrune || entry.Subject != "retention" || entry.OK ||
		entry.IdempotencyKey != request.IdempotencyKey || entry.RequestDigest != "" || entry.Reason != "" ||
		entry.Detail != wantDetail || entry.SourceAddr != request.Actor.SourceAddr ||
		entry.AuthSubject != request.Actor.AuthSubject || entry.AuthCapability != request.Actor.AuthCapability ||
		entry.AuthDecision != request.Actor.AuthDecision || entry.SourceKind != SourceKindWeb {
		t.Fatalf("transport audit shape=%+v", entry)
	}

	request.Code = RetentionTransportRejectionCode("FREE_FORM_DETAIL")
	if err := service.RecordRetentionTransportRejection(request); err == nil {
		t.Fatal("operator service accepted an unbounded retention rejection code")
	}
	entries, err = st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("invalid code wrote an audit row: %+v err=%v", entries, err)
	}
}
