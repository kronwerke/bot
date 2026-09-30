package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestForgetKeepsOpenApplicationsAndOthers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetLink("1", "Anna_MC")
	s.SetLink("2", "Ben_MC")
	s.MarkVerified("1")
	s.Fail("1", 3, time.Minute)
	s.PutApplication(Application{UserID: "1", Status: "declined"})
	s.PutApplication(Application{UserID: "2", Status: "open", ChannelID: "55"})

	s.Forget("1")
	s.Forget("2")
	if s.Link("1") != "" || s.VerifiedCount() != 0 {
		t.Fatal("link or verification of 1 kept")
	}
	if _, ok := s.Application("1"); ok {
		t.Fatal("decided application of 1 kept")
	}
	if _, ok := s.Application("2"); !ok {
		t.Fatal("open application of 2 dropped")
	}
	if s.Link("2") != "" {
		t.Fatal("link of 2 kept")
	}
}

func TestDropSweptApplications(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.PutApplication(Application{UserID: "1", Status: "accepted"})
	s.PutApplication(Application{UserID: "2", Status: "declined", ChannelID: "7"})
	s.PutApplication(Application{UserID: "3", Status: "open", ChannelID: "8"})
	if n := s.DropSweptApplications(); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	if len(s.Applications()) != 2 {
		t.Fatal("wrong applications left")
	}
}
