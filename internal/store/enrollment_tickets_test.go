package store

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func issueTicket(t *testing.T, s *Store, name string, at time.Time, ttl time.Duration) string {
	t.Helper()
	previous := s.nowFn
	s.nowFn = func() time.Time { return at.UTC() }
	defer func() { s.nowFn = previous }()
	machineID, _, err := s.CreateEnrollTokenFor(name, ttl)
	if err != nil {
		t.Fatalf("開票 %s: %v", name, err)
	}
	return machineID
}

func issueAndRedeem(t *testing.T, s *Store, name string, at time.Time) string {
	t.Helper()
	previous := s.nowFn
	s.nowFn = func() time.Time { return at.UTC() }
	machineID, token, err := s.CreateEnrollTokenFor(name, time.Hour)
	s.nowFn = previous
	if err != nil {
		t.Fatalf("開票 %s: %v", name, err)
	}
	if _, _, err := s.RedeemEnrollToken(token, model.EnrollRequest{Hostname: name}, at.UTC()); err != nil {
		t.Fatalf("兌換 %s: %v", name, err)
	}
	return machineID
}

// 一張還沒用掉的票要說得出它是什麼時候開的、什麼時候到期，以及現在過期了沒有。
func TestAPendingTicketSaysWhenItWasIssuedAndWhetherItHasExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	live := issueTicket(t, s, "fresh", now.Add(-30*time.Minute), time.Hour)
	dead := issueTicket(t, s, "stale", now.Add(-3*time.Hour), time.Hour)

	tickets, err := s.PendingEnrollmentTickets(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 2 {
		t.Fatalf("讀到 %d 台有未用票，開了 2 張", len(tickets))
	}
	if got := tickets[live]; got.Pending != 1 || got.Expired != 0 || got.NewestExpired {
		t.Errorf("還有效的票被算成過期：%+v", got)
	}
	if got := tickets[dead]; got.Pending != 1 || got.Expired != 1 || !got.NewestExpired {
		t.Errorf("過期的票被算成還有效：%+v", got)
	}
	if got := tickets[dead].ExpiresAt; !got.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("到期時刻=%s", got)
	}
}

// 用掉的票不是「還沒用掉的票」。票一旦兌換，這台機器就不該再出現在待兌換清單裡。
func TestARedeemedTicketIsNoLongerPending(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	machineID := issueAndRedeem(t, s, "arrived", now.Add(-time.Hour))

	tickets, err := s.PendingEnrollmentTickets(now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tickets[machineID]; ok {
		t.Errorf("兌換過的票還留在待兌換清單裡：%+v", tickets[machineID])
	}
}

// 解不出來的 expires_at 一律算過期，跟兌換那一條路同一條規則。一個壞掉的時間戳
// 在畫面上變成「還有效」，等於邀請一張永遠不會到期的票。
func TestATicketWithAnUnreadableExpiryCountsAsExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	machineID := issueTicket(t, s, "broken", now.Add(-time.Minute), time.Hour)
	for _, bad := range []string{"", "2026-09-12 16:00:00", "2026-09-12T16:00:00+08:00", "not a time"} {
		if _, err := s.db.Exec(
			`UPDATE enrollment_tokens SET expires_at=? WHERE used_by=?`, bad, machineID); err != nil {
			t.Fatal(err)
		}
		tickets, err := s.PendingEnrollmentTickets(now)
		if err != nil {
			t.Fatal(err)
		}
		if got := tickets[machineID]; got.Expired != 1 || !got.NewestExpired {
			t.Errorf("expires_at=%q 被算成還有效：%+v", bad, got)
		}
	}
}

// 同一台機器身上有一張以上的未用票時，畫面挑的是最新的那一張——跟單機頁挑的
// 必須是同一張，否則兩頁會對同一台機器講出不同的到期時刻。
func TestTheNewestPendingTicketIsTheOneDescribed(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	machineID := issueTicket(t, s, "twice", now.Add(-5*time.Hour), time.Hour)
	if _, err := s.db.Exec(`
INSERT INTO enrollment_tokens (token_hash, display_name, created_at, expires_at, used_by)
VALUES ('later-hash','twice',?,?,?)`,
		fmtTime(now.Add(-10*time.Minute)), fmtTime(now.Add(50*time.Minute)), machineID); err != nil {
		t.Fatal(err)
	}
	tickets, err := s.PendingEnrollmentTickets(now)
	if err != nil {
		t.Fatal(err)
	}
	got := tickets[machineID]
	if got.Pending != 2 || got.Expired != 1 {
		t.Fatalf("兩張票的計數=%+v", got)
	}
	if got.NewestExpired {
		t.Error("最新那一張還有效，卻被講成過期")
	}
	if !got.CreatedAt.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("挑到的不是最新那一張：%s", got.CreatedAt)
	}

	pending, err := s.PendingEnrollToken(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil || !pending.ExpiresAt.Equal(got.ExpiresAt) || pending.Expired != got.NewestExpired {
		t.Errorf("機隊那一份挑的票跟單機頁不一樣：fleet=%+v machine=%+v", got, pending)
	}
}

// 讀不完就不回一份看起來完整的清單。
func TestTooManyPendingTicketsIsRefusedNotTruncated(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	machineID := issueTicket(t, s, "many", now.Add(-time.Minute), time.Hour)
	for i := 0; i <= MaxPendingEnrollmentTickets; i++ {
		if _, err := s.db.Exec(`
INSERT INTO enrollment_tokens (token_hash, display_name, created_at, expires_at, used_by)
VALUES (?,'many',?,?,?)`,
			"hash-"+time.Duration(i).String(), fmtTime(now), fmtTime(now.Add(time.Hour)), machineID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PendingEnrollmentTickets(now); err == nil {
		t.Error("票多到讀不完，卻回了一份清單")
	}
}

// 沒有可信的時鐘就答不出「過期了沒有」，所以它拒絕，而不是拿一個零值當現在。
func TestPendingTicketsNeedATrustedClock(t *testing.T) {
	if _, err := newTestStore(t).PendingEnrollmentTickets(time.Time{}); err == nil {
		t.Error("沒有時間也回了待兌換清單")
	}
}

// 單機那一頁與機隊那一份報告必須對同一張票給出同一個答案。兩邊各寫一次 SQL 的話，
// 一個解不出來的 expires_at 會在一頁上算過期、在另一頁上算還有效——而操作員照著
// 其中一頁決定要不要重開票。
func TestBothPagesCallTheSameTicketExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	machineID := issueTicket(t, s, "drift", now.Add(-time.Minute), time.Hour)
	for _, expiry := range []string{
		fmtTime(now.Add(time.Hour)), fmtTime(now.Add(-time.Hour)),
		"", "2026-09-12 16:00:00", "2026-09-12T16:00:00+08:00", "not a time",
	} {
		if _, err := s.db.Exec(
			`UPDATE enrollment_tokens SET expires_at=? WHERE used_by=?`, expiry, machineID); err != nil {
			t.Fatal(err)
		}
		tickets, err := s.PendingEnrollmentTickets(now)
		if err != nil {
			t.Fatal(err)
		}
		lifecycle, err := s.OperatorMachineLifecycle(machineID)
		if err != nil {
			t.Fatal(err)
		}
		fleet := int64(tickets[machineID].Expired)
		if fleet != lifecycle.PendingEnrollmentTokenExpiredCount {
			t.Errorf("expires_at=%q：機隊說過期 %d 張，單機頁說 %d 張",
				expiry, fleet, lifecycle.PendingEnrollmentTokenExpiredCount)
		}
	}
}
