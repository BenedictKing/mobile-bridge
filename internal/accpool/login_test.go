package accpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLoginDriver registers a driver whose wait closure blocks on a per-start
// channel, letting tests drive the login lifecycle deterministically.
type fakeLoginDriver struct {
	chans []chan fakeLoginOutcome
}

type fakeLoginOutcome struct {
	account *Account
	err     error
}

func (f *fakeLoginDriver) start(_ context.Context, _ *Manager, _ LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error) {
	waitCh := make(chan fakeLoginOutcome, 1)
	f.chans = append(f.chans, waitCh)
	return &LoginChallenge{UserCode: "ABCD-1234", URL: "https://example.com/activate", ExpiresIn: 600},
		func(ctx context.Context) (*Account, error) {
			select {
			case outcome := <-waitCh:
				return outcome.account, outcome.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, nil
}

func (f *fakeLoginDriver) feed(outcome fakeLoginOutcome) {
	f.chans[len(f.chans)-1] <- outcome
}

func loginTestManager(t *testing.T) *Manager {
	t.Helper()
	m := New(t.TempDir(), "")
	m.now = func() time.Time { return time.UnixMilli(1700000000000) }
	return m
}

func waitLoginStatus(t *testing.T, m *Manager, kind, want string) *LoginSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		session, err := m.LoginStatus(kind)
		if err == nil && session.Status == want {
			return session
		}
		time.Sleep(10 * time.Millisecond)
	}
	session, _ := m.LoginStatus(kind)
	t.Fatalf("login never reached %s (last: %+v)", want, session)
	return nil
}

func TestLoginDoneUpsertsAccount(t *testing.T) {
	m := loginTestManager(t)
	driver := &fakeLoginDriver{}
	registerLoginDriver("kimi", driver.start)
	defer delete(loginDrivers, "kimi")

	session, challenge, err := m.StartLogin(context.Background(), "kimi", LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "ABCD-1234" || challenge.URL == "" {
		t.Fatalf("unexpected challenge: %+v", challenge)
	}
	if session.Status != LoginPending {
		t.Fatalf("status = %s", session.Status)
	}
	driver.feed(fakeLoginOutcome{account: &Account{
		UserID: "u_1", Email: "a@b.c", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: m.now().Add(time.Hour).UnixMilli(),
	}})
	done := waitLoginStatus(t, m, "kimi", LoginDone)
	if done.AccountID == "" {
		t.Fatal("done login carried no account id")
	}
	accounts := m.list("kimi")
	if len(accounts) != 1 || accounts[0].AccessToken != "at" {
		t.Fatalf("pool = %+v", accounts)
	}
}

func TestLoginFailureSurfacesError(t *testing.T) {
	m := loginTestManager(t)
	driver := &fakeLoginDriver{}
	registerLoginDriver("minimax", driver.start)
	defer delete(loginDrivers, "minimax")

	if _, _, err := m.StartLogin(context.Background(), "minimax", LoginOptions{}); err != nil {
		t.Fatal(err)
	}
	driver.feed(fakeLoginOutcome{err: errors.New("用户拒绝了授权")})
	failed := waitLoginStatus(t, m, "minimax", LoginFailed)
	if failed.Error != "用户拒绝了授权" {
		t.Fatalf("error = %q", failed.Error)
	}
	if n := len(m.list("minimax")); n != 0 {
		t.Fatalf("failed login persisted %d accounts", n)
	}
}

func TestLoginCancel(t *testing.T) {
	m := loginTestManager(t)
	driver := &fakeLoginDriver{}
	registerLoginDriver("workbuddy", driver.start)
	defer delete(loginDrivers, "workbuddy")

	if _, _, err := m.StartLogin(context.Background(), "workbuddy", LoginOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CancelLogin("workbuddy"); err != nil {
		t.Fatal(err)
	}
	cancelled := waitLoginStatus(t, m, "workbuddy", LoginCancelled)
	if cancelled.Error == "" {
		t.Fatal("cancelled login carried no message")
	}
}

func TestLoginSupersededByNewLogin(t *testing.T) {
	m := loginTestManager(t)
	driver := &fakeLoginDriver{}
	registerLoginDriver("zcode", driver.start)
	defer delete(loginDrivers, "zcode")

	first, _, err := m.StartLogin(context.Background(), "zcode", LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := m.StartLogin(context.Background(), "zcode", LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("login id reused")
	}
	// The first wait goroutine observes the cancel and must not overwrite the
	// new pending session — feed the superseded login's own channel.
	driver.chans[0] <- fakeLoginOutcome{account: &Account{UserID: "u_old", AccessToken: "stale"}}
	time.Sleep(100 * time.Millisecond)
	current, err := m.LoginStatus("zcode")
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != LoginPending || current.ID != second.ID {
		t.Fatalf("superseded login clobbered the new session: %+v", current)
	}
	if n := len(m.list("zcode")); n != 0 {
		t.Fatalf("stale login persisted %d accounts", n)
	}
}

func TestLoginReauthReplacesTokensInPlace(t *testing.T) {
	m := loginTestManager(t)
	driver := &fakeLoginDriver{}
	registerLoginDriver("kimi", driver.start)
	defer delete(loginDrivers, "kimi")

	stale := &Account{
		ID: "u_stale", UserID: "u_stale", Email: "old@x.y",
		AccessToken: "old-at", RefreshToken: "old-rt",
		Status: StatusReauthRequired, StatusReason: "刷新失败",
	}
	if err := m.saveAccount("kimi", stale); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.StartLogin(context.Background(), "kimi", LoginOptions{ReauthID: "u_stale"}); err != nil {
		t.Fatal(err)
	}
	driver.feed(fakeLoginOutcome{account: &Account{
		UserID: "u_stale", AccessToken: "new-at", RefreshToken: "new-rt",
		ExpiresAt: m.now().Add(time.Hour).UnixMilli(),
	}})
	done := waitLoginStatus(t, m, "kimi", LoginDone)
	if done.AccountID != "u_stale" {
		t.Fatalf("reauth landed on %q", done.AccountID)
	}
	reloaded, err := m.loadAccount("kimi", "u_stale")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AccessToken != "new-at" || reloaded.RefreshToken != "new-rt" {
		t.Fatalf("tokens not replaced: %+v", reloaded)
	}
	if reloaded.Status != StatusActive || reloaded.StatusReason != "" {
		t.Fatalf("status = %s (%s)", reloaded.Status, reloaded.StatusReason)
	}
	if reloaded.Email != "old@x.y" {
		t.Fatalf("identity lost: %+v", reloaded)
	}
	if n := len(m.list("kimi")); n != 1 {
		t.Fatalf("reauth created extra accounts: %d", n)
	}
}

func TestLoginReauthRequiresExistingAccount(t *testing.T) {
	m := loginTestManager(t)
	if _, _, err := m.StartLogin(context.Background(), "kimi", LoginOptions{ReauthID: "missing"}); err == nil {
		t.Fatal("reauth against a missing account must fail")
	}
}

func TestLoginUnknownKind(t *testing.T) {
	m := loginTestManager(t)
	if _, _, err := m.StartLogin(context.Background(), "codex", LoginOptions{}); err == nil {
		t.Fatal("unsupported kind must fail")
	}
	if _, _, err := m.StartLogin(context.Background(), "kimi", LoginOptions{}); err == nil {
		t.Fatal("kind without a registered driver must fail")
	}
}
