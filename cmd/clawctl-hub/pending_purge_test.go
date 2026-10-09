package main

import (
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestPurgePendingLoginsForAccount(t *testing.T) {
	p := newPendingLogins()
	alice, err := p.add(store.HubAccount{AccountID: "acct-alice", Username: "alice"}, "192.0.2.1", "/")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := p.add(store.HubAccount{AccountID: "acct-bob", Username: "bob"}, "192.0.2.1", "/")
	if err != nil {
		t.Fatal(err)
	}
	purgePendingLoginsForAccount("acct-alice")
	if _, ok := p.get(alice, "192.0.2.1"); ok {
		t.Fatal("pending login for a disabled account survived")
	}
	if p.consume(alice, "192.0.2.1") {
		t.Fatal("purged pending login could still be consumed")
	}
	if _, ok := p.get(bob, "192.0.2.1"); !ok {
		t.Fatal("other accounts' pending logins must survive")
	}
	purgePendingLoginsForAccount("")
	if _, ok := p.get(bob, "192.0.2.1"); !ok {
		t.Fatal("empty account id must not purge anything")
	}
}
