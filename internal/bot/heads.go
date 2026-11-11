package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// heads looks Minecraft accounts up at Mojang and cuts the face out of their skin, for the
// whitelist app. Discord only loads images through the app's own address, so the faces
// come from here and not from a head service. Everything is cached: names for six hours,
// unknown names for ten minutes.
type heads struct {
	profileAPI string // https://api.mojang.com/users/profiles/minecraft/
	sessionAPI string // https://sessionserver.mojang.com/session/minecraft/profile/
	textures   string // https://textures.minecraft.net/texture/, the only place skins come from
	http       *http.Client

	mu      sync.Mutex
	entries map[string]*headEntry // lower case name
}

type headEntry struct {
	at    time.Time
	found bool
	name  string // as Mojang spells it
	uuid  string
	png   []byte // nil until the face was asked for
	wait  chan struct{}
}

const (
	headSize     = 64 // pixels of the face that is served
	headTTL      = 6 * time.Hour
	headMissTTL  = 10 * time.Minute
	headCacheMax = 4000
)

func newHeads() *heads {
	return &heads{
		profileAPI: "https://api.mojang.com/users/profiles/minecraft/",
		sessionAPI: "https://sessionserver.mojang.com/session/minecraft/profile/",
		textures:   "https://textures.minecraft.net/texture/",
		http:       &http.Client{Timeout: 8 * time.Second},
		entries:    map[string]*headEntry{},
	}
}

var errNoAccount = errors.New("no Minecraft account with that name")

// lookup returns the account behind a name, from the cache or from Mojang. One request
// per name at a time; the others wait for it.
func (h *heads) lookup(ctx context.Context, name string) (*headEntry, error) {
	if !mcName.MatchString(name) {
		return nil, errNoAccount
	}
	key := strings.ToLower(name)
	h.mu.Lock()
	e := h.entries[key]
	if e != nil {
		if e.wait != nil {
			ch := e.wait
			h.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return h.lookup(ctx, name)
		}
		ttl := headTTL
		if !e.found {
			ttl = headMissTTL
		}
		if time.Since(e.at) < ttl {
			h.mu.Unlock()
			if !e.found {
				return nil, errNoAccount
			}
			return e, nil
		}
	}
	if len(h.entries) >= headCacheMax {
		h.entries = map[string]*headEntry{}
	}
	e = &headEntry{wait: make(chan struct{})}
	h.entries[key] = e
	h.mu.Unlock()

	found, canon, uuid, err := h.profile(ctx, name)
	h.mu.Lock()
	close(e.wait)
	e.wait = nil
	if err != nil {
		delete(h.entries, key) // a failure at Mojang is not remembered
		h.mu.Unlock()
		return nil, err
	}
	e.at, e.found, e.name, e.uuid = time.Now(), found, canon, uuid
	h.mu.Unlock()
	if !found {
		return nil, errNoAccount
	}
	return e, nil
}

func (h *heads) profile(ctx context.Context, name string) (found bool, canon, uuid string, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.profileAPI+name, nil)
	resp, err := h.http.Do(req)
	if err != nil {
		return false, "", "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent, http.StatusNotFound:
		return false, "", "", nil
	default:
		return false, "", "", fmt.Errorf("mojang answered %d", resp.StatusCode)
	}
	var p struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&p); err != nil || len(p.ID) != 32 {
		return false, "", "", fmt.Errorf("mojang: unexpected answer")
	}
	return true, p.Name, dashed(p.ID), nil
}

// dashed turns Mojang's 32 hex digits into the usual UUID form.
func dashed(id string) string {
	id = strings.ToLower(id)
	if len(id) != 32 {
		return id
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// face returns the PNG of the account's face, hat layer on top. Accounts without a
// skin of their own, and failures at Mojang, get the plain face.
func (h *heads) face(ctx context.Context, name string) ([]byte, bool) {
	e, err := h.lookup(ctx, name)
	if err != nil {
		return placeholderFace(), false
	}
	h.mu.Lock()
	if e.png != nil {
		p := e.png
		h.mu.Unlock()
		return p, true
	}
	h.mu.Unlock()
	skin, err := h.skin(ctx, e.uuid)
	if err != nil {
		return placeholderFace(), true
	}
	p, err := cutFace(skin)
	if err != nil {
		return placeholderFace(), true
	}
	h.mu.Lock()
	e.png = p
	h.mu.Unlock()
	return p, true
}

func (h *heads) skin(ctx context.Context, uuid string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.sessionAPI+strings.ReplaceAll(uuid, "-", ""), nil)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session server answered %d", resp.StatusCode)
	}
	var prof struct {
		Properties []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&prof); err != nil {
		return nil, err
	}
	var url string
	for _, p := range prof.Properties {
		if p.Name != "textures" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(p.Value)
		if err != nil {
			return nil, err
		}
		var t struct {
			Textures struct {
				Skin struct {
					URL string `json:"url"`
				} `json:"SKIN"`
			} `json:"textures"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		url = t.Textures.Skin.URL
	}
	// only Mojang's texture server: the address comes from an answer, not from us
	if strings.HasPrefix(url, "http://textures.minecraft.net/") {
		url = "https://" + strings.TrimPrefix(url, "http://")
	}
	if !strings.HasPrefix(url, h.textures) {
		return nil, errors.New("no skin")
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	r2, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("texture server answered %d", r2.StatusCode)
	}
	return io.ReadAll(io.LimitReader(r2.Body, 512<<10))
}

// cutFace takes the 8x8 face and the hat layer over it from a skin and scales them up
// without smoothing.
func cutFace(skin []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(skin))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() < 64 || b.Dy() < 32 {
		return nil, errors.New("not a skin")
	}
	unit := b.Dx() / 64 // high resolution skins
	face := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			base := color.NRGBAModel.Convert(img.At(b.Min.X+(8+x)*unit, b.Min.Y+(8+y)*unit)).(color.NRGBA)
			base.A = 255
			face.SetNRGBA(x, y, base)
		}
	}
	hat := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	// old 64x32 skins often fill the hat layer with one opaque colour; Minecraft draws no
	// hat then, and neither do we
	opaque := b.Dy() < b.Dx()
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+(40+x)*unit, b.Min.Y+(8+y)*unit)).(color.NRGBA)
			hat.SetNRGBA(x, y, c)
			if c.A < 255 {
				opaque = false
			}
		}
	}
	if !(opaque && b.Dy() < b.Dx()) {
		draw.Draw(face, face.Bounds(), hat, image.Point{}, draw.Over)
	}
	return scaled(face)
}

func scaled(face *image.NRGBA) ([]byte, error) {
	out := image.NewNRGBA(image.Rect(0, 0, headSize, headSize))
	k := headSize / 8
	for y := 0; y < headSize; y++ {
		for x := 0; x < headSize; x++ {
			out.SetNRGBA(x, y, face.NRGBAAt(x/k, y/k))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var (
	placeholderOnce sync.Once
	placeholderPNG  []byte
)

// placeholderFace is a plain face in the colours of the website, for names without an account.
func placeholderFace() []byte {
	placeholderOnce.Do(func() {
		f := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		skin := color.NRGBA{0x4a, 0x42, 0x4f, 255}
		dark := color.NRGBA{0x2c, 0x26, 0x31, 255}
		eye := color.NRGBA{0xaa, 0xa3, 0x9b, 255}
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				c := skin
				if y < 2 {
					c = dark
				}
				f.SetNRGBA(x, y, c)
			}
		}
		f.SetNRGBA(2, 4, eye)
		f.SetNRGBA(5, 4, eye)
		for x := 3; x <= 4; x++ {
			f.SetNRGBA(x, 6, dark)
		}
		placeholderPNG, _ = scaled(f)
	})
	return placeholderPNG
}
