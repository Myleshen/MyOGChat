package auth

import "testing"

func TestHashPasswordRoundTrip(t *testing.T) {
	const pw = "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(h, pw) {
		t.Error("valid password was rejected")
	}
	if VerifyPassword(h, "wrong password") {
		t.Error("wrong password was accepted")
	}
	if VerifyPassword("garbage", pw) || VerifyPassword("", pw) {
		t.Error("malformed hash was accepted")
	}
	// Random salt means the same password hashes differently each time.
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h == h2 {
		t.Error("two hashes of the same password are identical (salt not random)")
	}
	if !VerifyPassword(h2, pw) {
		t.Error("second hash failed to verify")
	}
}

func TestValidateCredentials(t *testing.T) {
	cases := []struct {
		name                  string
		username, email, pass string
		want                  bool
	}{
		{"valid", "alice", "alice@example.com", "supersecret", true},
		{"username too short", "ab", "a@b.co", "supersecret", false},
		{"username bad char", "my leshen", "a@b.co", "supersecret", false},
		{"email missing @", "alice", "not-an-email", "supersecret", false},
		{"email missing dot", "alice", "a@b", "supersecret", false},
		{"password too short", "alice", "a@b.co", "short", false},
		{"valid dots and dashes", "my-leshen.99", "x@y.z", "longenough", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := ValidateCredentials(c.username, c.email, c.pass); ok != c.want {
				t.Errorf("ValidateCredentials(%q,%q,%q) = %v, want %v",
					c.username, c.email, c.pass, ok, c.want)
			}
		})
	}
}
