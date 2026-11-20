package mcauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"

	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerHashMatchesMinecraft(t *testing.T) {
	// the known examples from the protocol documentation: sha1 of a name, printed signed
	for name, want := range map[string]string{
		"Notch": "4ed1f46bbe04bc756bcb17c0c7ce3e4632f06a48",
		"jeb_":  "-7c9d5b0044c130109a5d7b5fb5c317c02b4e28c1",
		"simon": "88e16a1019277b15d58faf0541e11910eb756f6",
	} {
		if got := serverHash(name, nil, nil); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestCFB8RoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	b, _ := aes.NewCipher(key)
	msg := []byte("Kronwerke sign in, a longer message than one block")
	enc := make([]byte, len(msg))
	newCFB8(b, key, false).XORKeyStream(enc, msg)
	dec := make([]byte, len(msg))
	newCFB8(b, key, true).XORKeyStream(dec, enc)
	if !bytes.Equal(dec, msg) {
		t.Fatalf("round trip: %q", dec)
	}
}

// a client that does what the game does: handshake, login start, encryption, read the kick
func signIn(t *testing.T, addr, name string, proto int32, join func(hash string)) string {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	c := &conn{c: nc, r: nc, w: nc}
	var hs bytes.Buffer
	writeVarInt(&hs, proto)
	writeString(&hs, "auth.kronwerke.com")
	hs.Write([]byte{0x63, 0xdd})
	writeVarInt(&hs, 2)
	c.write(0x00, hs.Bytes())
	var ls bytes.Buffer
	writeString(&ls, name)
	ls.Write(make([]byte, 16))
	c.write(0x00, ls.Bytes())
	id, body, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if id == 0x00 { // kicked before encryption
		s, _ := readString(body, 1<<15)
		return s
	}
	if id != 0x01 {
		t.Fatalf("packet %d", id)
	}
	serverID, _ := readString(body, 20)
	pubDER, _ := readBytes(body, 1024)
	token, _ := readBytes(body, 16)
	pub, err := x509.ParsePKIXPublicKey(pubDER)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	join(serverHash(serverID, secret, pubDER))
	encS, _ := rsa.EncryptPKCS1v15(rand.Reader, pub.(*rsa.PublicKey), secret)
	encT, _ := rsa.EncryptPKCS1v15(rand.Reader, pub.(*rsa.PublicKey), token)
	var er bytes.Buffer
	writeVarInt(&er, int32(len(encS)))
	er.Write(encS)
	writeVarInt(&er, int32(len(encT)))
	er.Write(encT)
	c.write(0x01, er.Bytes())
	blk, _ := aes.NewCipher(secret)
	c.r = cipher.StreamReader{S: newCFB8(blk, secret, true), R: nc}
	id, body, err = c.read()
	if err != nil || id != 0x00 {
		t.Fatalf("kick: %d %v", id, err)
	}
	s, _ := readString(body, 1<<15)
	return s
}

func start(t *testing.T, s *Server) string {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ListenAndServe(ctx)
	for i := 0; i < 100; i++ {
		if a := s.Listening(); a != nil {
			return a.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("not listening")
	return ""
}

func TestSignInGivesTheCodeOfTheAccountMojangConfirms(t *testing.T) {
	joined := ""
	mojang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("serverId") != joined || r.URL.Query().Get("username") != "Elchi_Sam" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		io.WriteString(w, `{"id":"bf8ccf0d901d4cdd88b66af144428974","name":"Elchi_Sam"}`)
	}))
	defer mojang.Close()
	var got Account
	s := &Server{Addr: "127.0.0.1:0", SessionURL: mojang.URL, OnAuth: func(a Account) (string, error) { got = a; return "K7Q2XP", nil }}
	addr := start(t, s)
	kick := signIn(t, addr, "Elchi_Sam", 767, func(h string) { joined = h })
	if got.UUID != "bf8ccf0d901d4cdd88b66af144428974" || got.Name != "Elchi_Sam" {
		t.Fatalf("account %+v", got)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(kick), &m); err != nil || !strings.Contains(kick, "K7Q2XP") {
		t.Fatalf("kick %q", kick)
	}
}

func TestSignInRefusedWhenMojangDoesNotKnowTheJoin(t *testing.T) {
	mojang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer mojang.Close()
	called := false
	s := &Server{Addr: "127.0.0.1:0", SessionURL: mojang.URL, OnAuth: func(a Account) (string, error) { called = true; return "X", nil }}
	addr := start(t, s)
	kick := signIn(t, addr, "Someone", 767, func(string) {})
	if called || !strings.Contains(kick, "Mojang") {
		t.Fatalf("called=%v kick=%q", called, kick)
	}
}

func TestOldClientsGetAMessage(t *testing.T) {
	s := &Server{Addr: "127.0.0.1:0", OnAuth: func(a Account) (string, error) { return "X", nil }}
	addr := start(t, s)
	kick := signIn(t, addr, "Old", 760, func(string) {})
	if !strings.Contains(kick, "1.20.2") {
		t.Fatalf("kick %q", kick)
	}
}
