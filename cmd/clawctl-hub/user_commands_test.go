package main

import (
	"strings"
	"testing"
)

func TestUserCommands(t *testing.T) {
	st := boundaryStore(t)
	var seq int
	var name, path string
	if err := st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	args := func(user string) []string { return []string{"--db", path, "--username", user} }
	add := func(user, email, password string) error {
		return runAdminPasswordCommand("add-admin", append(args(user), "--email", email), strings.NewReader(password))
	}
	for _, password := range []string{"short", strings.Repeat("x", 259)} {
		if err := add("alice", "alice@example.com", password); err == nil {
			t.Fatal("invalid password")
		}
	}
	if err := add(" ALICE ", " ALICE@Example.com ", usersPassword+"\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := add("bob", "bob@example.com", usersPassword); err != nil {
		t.Fatal(err)
	}
	if err := add("carol", "alice@example.com", usersPassword); err == nil {
		t.Fatal("duplicate email")
	}
	if err := runUserCommand("set-email", append(args("bob"), "--email", "alice@example.com")); err == nil {
		t.Fatal("duplicate email update")
	}
	if err := runUserCommand("set-email", append(args("bob"), "--email", "invalid")); err == nil {
		t.Fatal("invalid email")
	}
	if err := runUserCommand("set-email", args("bob")); err == nil {
		t.Fatal("missing email flag cleared")
	}
	if err := runUserCommand("set-email", append(args("bob"), "--email", "")); err != nil {
		t.Fatal(err)
	}
	bob, err := st.VerifyPassword("bob", usersPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateSession(bob, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = runUserCommand("rename-user", append(args("bob"), "--new-username", "carol")); err != nil {
		t.Fatal(err)
	}
	if a, err := st.LookupSession(token); err != nil || a.Username != "carol" {
		t.Fatal(a, err)
	}
	if err = runResetAdminPassword(args("carol"), strings.NewReader("a replacement password")); err != nil {
		t.Fatal(err)
	}
	if _, err = st.LookupSession(token); err == nil {
		t.Fatal("reset left session")
	}
	if _, err = st.VerifyPassword("carol", "a replacement password", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err = runResetAdminPassword(args("missing"), strings.NewReader(usersPassword)); err == nil || !strings.Contains(err.Error(), "account not found") {
		t.Fatal(err)
	}
	if err = runUserCommand("disable-user", args("carol")); err != nil {
		t.Fatal(err)
	}
	if err = runUserCommand("disable-user", args("alice")); err == nil {
		t.Fatal("last admin disabled")
	}
	if err = runUserCommand("enable-user", args("carol")); err != nil {
		t.Fatal(err)
	}
	if _, err = st.VerifyPassword("carol", "a replacement password", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"add-admin", "set-email", "rename-user", "disable-user", "enable-user"} {
		if got, err := classifyTopLevel([]string{command}); err != nil || got != command {
			t.Fatal(got, err)
		}
	}
}
