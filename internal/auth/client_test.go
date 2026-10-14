package auth

import "testing"

func TestClientWalksForwardedForFromTheRight(t *testing.T) {
	auth := NewAuth(Config{Mode: "none"})

	forwarded := auth.Client("127.0.0.1", "bridge.example", "203.0.113.5, 127.0.0.1", "", true)
	if forwarded.MustGet("ip") != "203.0.113.5" || forwarded.MustGet("source") != "forwarded" {
		t.Fatalf("walk: %v", forwarded.MustGet("ip"))
	}

	// The first untrusted hop from the right is the client.
	untrusted := auth.Client("127.0.0.1", "bridge.example", "203.0.113.5, 198.51.100.7", "", true)
	if untrusted.MustGet("ip") != "198.51.100.7" {
		t.Fatalf("untrusted walk: %v", untrusted.MustGet("ip"))
	}

	quick := auth.Client("127.0.0.1", "x.trycloudflare.com", "ignored", "203.0.113.9", true)
	if quick.MustGet("ip") != "203.0.113.9" {
		t.Fatalf("cf-connecting-ip: %v", quick.MustGet("ip"))
	}

	broken := auth.Client("127.0.0.1", "bridge.example", "1.2.3.4, not-an-ip", "", true)
	if broken.MustGet("ip") != "127.0.0.1" || broken.MustGet("source") != "proxy" {
		t.Fatalf("malformed chain: %v %v", broken.MustGet("ip"), broken.MustGet("source"))
	}

	direct := auth.Client("192.168.1.4", "bridge.example", "203.0.113.5", "", false)
	if direct.MustGet("ip") != "192.168.1.4" || direct.MustGet("source") != "direct" {
		t.Fatalf("insecure: %v %v", direct.MustGet("ip"), direct.MustGet("source"))
	}
}

func TestLoginClientRecordsForwardedIdentity(t *testing.T) {
	auth := NewAuth(Config{Mode: "none"})
	client := auth.Client("127.0.0.1", "bridge.example", "203.0.113.5, 127.0.0.1", "", true)
	token, session, err := auth.LoginClient("admin", "", client, "iPhone", nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.MustGet("ip") != "203.0.113.5" || session.MustGet("peer") != "127.0.0.1" || session.MustGet("source") != "forwarded" {
		t.Fatalf("session client: ip=%v peer=%v source=%v", session.MustGet("ip"), session.MustGet("peer"), session.MustGet("source"))
	}
	if auth.Get(token) == nil {
		t.Fatal("session unusable")
	}
}
