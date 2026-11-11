package bot

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kronwerke/bot/internal/store"
)

func TestMissingFindsPlacesTheServerLost(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st}
	now := time.Now()
	st.SetLink("1", "Carla")
	st.SetLink("2", "Dora")
	st.PutGrant(store.Grant{Player: "Carla", DiscordID: "1", Kind: store.KindStreamer, Created: now})
	st.PutGrant(store.Grant{Player: "Dora", DiscordID: "2", Kind: store.KindSeason1, Created: now})
	st.PutInvite(store.Invite{Player: "Ben", DiscordID: "3", StreamerID: "1", Created: now})
	st.PutInvite(store.Invite{Player: "Eva", DiscordID: "4", StreamerID: "1", Created: now})

	var r rosterView
	r.Streamers = append(r.Streamers, rosterStreamer{Name: "carla", Granted: true, Invited: []rosterPlayer{{Name: "BEN"}}})

	grants, invites := b.missing(&r)
	if len(grants) != 1 || grants[0].Player != "Dora" {
		t.Fatalf("grants: %+v", grants)
	}
	if len(invites) != 1 || invites[0].Player != "Eva" {
		t.Fatalf("invites: %+v", invites)
	}

	grants, invites = b.missing(&rosterView{})
	if len(grants) != 2 || len(invites) != 2 {
		t.Fatalf("a new world lost everything: %d grants, %d invites", len(grants), len(invites))
	}
}

// A renamed account keeps its place: the bot knows it by UUID, takes the new name from the
// roster and finds nothing missing.
func TestRenamedAccountsKeepTheirPlaces(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &Bot{store: st}
	now := time.Now()
	carla := "11111111-1111-1111-1111-111111111111"
	ben := "22222222-2222-2222-2222-222222222222"
	st.SetLink("1", "Carla")
	st.SetLink("3", "Ben")
	st.PutGrant(store.Grant{Player: "Carla", DiscordID: "1", Kind: store.KindStreamer, Created: now})
	st.PutInvite(store.Invite{Player: "Ben", DiscordID: "3", StreamerID: "1", Created: now})

	// the first pass learns the UUIDs by name
	r := &rosterView{Streamers: []rosterStreamer{{UUID: carla, Name: "Carla", Granted: true, Invited: []rosterPlayer{{UUID: ben, Name: "Ben"}}}}}
	b.learnFrom(r)
	if g, _ := st.Grant("1"); g.UUID != carla {
		t.Fatalf("grant uuid: %+v", g)
	}
	if i, _ := st.Invite("Ben"); i.UUID != ben {
		t.Fatalf("invite uuid: %+v", i)
	}

	// both rename; Core shows the new names under the same UUIDs
	r = &rosterView{Streamers: []rosterStreamer{{UUID: carla, Name: "CarlaTV", Granted: true, Invited: []rosterPlayer{{UUID: ben, Name: "Benno"}}}}}
	out := b.learnFrom(r)
	if out == "" {
		t.Fatal("no rename reported")
	}
	if g, _ := st.Grant("1"); g.Player != "CarlaTV" || st.Link("1") != "CarlaTV" {
		t.Fatalf("grant after rename: %+v, link %q", g, st.Link("1"))
	}
	if i, ok := st.Invite("Benno"); !ok || i.UUID != ben {
		t.Fatalf("invite after rename: %+v %v", i, ok)
	}
	if _, ok := st.Invite("Ben"); ok {
		t.Fatal("old name still holds the invite")
	}
	grants, invites := b.missing(r)
	if len(grants) != 0 || len(invites) != 0 {
		t.Fatalf("missing after rename: %+v %+v", grants, invites)
	}
	if got := b.streamerRef("1"); got != carla {
		t.Fatalf("streamer ref %q", got)
	}
}

func TestAnswerUUID(t *testing.T) {
	if got := answerUUID("slots used 1/5 uuid 069A79F4-44E9-4726-A5BE-FCA90E38AAF5"); got != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Fatalf("got %q", got)
	}
	if got := grantCommand("Carla", -1, store.KindStreamer, ""); got != "kw admin grant Carla -1 streamer" {
		t.Fatalf("got %q", got)
	}
}
