package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateSurvivesReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSetting("channel.logs", "123")
	s.PutInvite(Invite{Player: "Anna_MC", DiscordID: "1", StreamerID: "9"})
	s.SetLink("9", "StreamerMC")

	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Setting("channel.logs") != "123" || s2.Link("9") != "StreamerMC" {
		t.Fatal("settings or links lost")
	}
	if i, ok := s2.Invite("anna_mc"); !ok || i.StreamerID != "9" {
		t.Fatal("invite lookup should ignore case")
	}
}

func TestBrokenFileIsNotOverwritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := Open(p); err == nil {
		t.Fatal("opened a broken state file")
	}
}

func TestLockoutAfterThreeFailures(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	for i := 1; i <= 2; i++ {
		if n, locked := s.Fail("u", 3, time.Minute); n != i || locked {
			t.Fatalf("attempt %d: n=%d locked=%v", i, n, locked)
		}
	}
	if _, locked := s.Fail("u", 3, time.Minute); !locked {
		t.Fatal("third failure should lock")
	}
	if time.Until(s.LockedUntil("u")) <= 0 {
		t.Fatal("lockout not stored")
	}
	s.MarkVerified("u")
	if !s.LockedUntil("u").IsZero() {
		t.Fatal("verification should clear the lockout")
	}
}

func TestTakeCaptchaIsSingleUse(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	s.SetCaptcha("u", Captcha{Code: "AC3K7", Expires: time.Now().Add(time.Minute)})
	if c, ok := s.TakeCaptcha("u"); !ok || c.Code != "AC3K7" {
		t.Fatal("captcha missing")
	}
	if _, ok := s.TakeCaptcha("u"); ok {
		t.Fatal("captcha used twice")
	}
}
