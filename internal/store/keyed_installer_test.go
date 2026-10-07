package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestPendingEnrollTicketMatchesDoesNotRedeem(t *testing.T) {
	st := newTestStore(t)
	id, token, err := st.CreateEnrollTokenFor("keyed-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	name, expiry, err := st.PendingEnrollTicketMatches(id, token, now)
	if err != nil || name != "keyed-machine" || !expiry.After(now) {
		t.Fatalf("match: name=%q expiry=%v err=%v", name, expiry, err)
	}
	for _, tc := range []struct {
		id, token string
		now       time.Time
	}{
		{id, "wrong", now}, {"other-machine", token, now}, {id, token, expiry},
	} {
		if _, _, err := st.PendingEnrollTicketMatches(tc.id, tc.token, tc.now); !errors.Is(err, ErrEnrollToken) {
			t.Fatalf("invalid match: %v", err)
		}
	}
	got, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{Hostname: "keyed-machine", OS: "linux", Arch: "amd64"}, now)
	if err != nil || got != id {
		t.Fatalf("redeem after match: id=%q err=%v", got, err)
	}
	if _, _, err := st.PendingEnrollTicketMatches(id, token, now); !errors.Is(err, ErrEnrollToken) {
		t.Fatalf("used ticket: %v", err)
	}
}
