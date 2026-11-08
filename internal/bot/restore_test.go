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
	r.Streamers = append(r.Streamers, struct {
		Name    string `json:"name"`
		Granted bool   `json:"granted"`
		Invited []struct {
			Name string `json:"name"`
		} `json:"invited"`
	}{Name: "carla", Granted: true, Invited: []struct {
		Name string `json:"name"`
	}{{Name: "BEN"}}})

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
