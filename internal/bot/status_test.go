package bot

import "testing"

func TestParseList(t *testing.T) {
	st, err := parseList("There are 2 of a max of 40 players online: Elchi_Sam, Anna_MC\n")
	if err != nil || st.Online != 2 || st.Max != 40 || len(st.Players) != 2 || st.Players[1] != "Anna_MC" {
		t.Fatalf("%+v %v", st, err)
	}
	st, err = parseList("There are 0 of a max of 20 players online: \n")
	if err != nil || st.Online != 0 || st.Max != 20 || len(st.Players) != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := parseList("Unknown command"); err == nil {
		t.Fatal("no error for a strange answer")
	}
}
