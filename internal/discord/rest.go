package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is the Discord REST base URL.
const DefaultAPI = "https://discord.com/api/v10"

// APIError is a non 2xx answer from Discord.
type APIError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
	Method  string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("discord: %s %s: %d %s (code %d)", e.Method, e.Path, e.Status, e.Message, e.Code)
}

// IsStatus reports whether err is an APIError with the HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// REST is a Discord REST client. Rate limits are respected per route: when a bucket is
// empty the next call on that route waits for its reset; a 429 is retried after
// retry_after.
type REST struct {
	Base   string
	token  string
	http   *http.Client
	ua     string
	mu     sync.Mutex
	blocks map[string]time.Time // route -> earliest next call
}

// NewREST creates a client for a bot token.
func NewREST(token, version string) *REST {
	return &REST{
		Base:   DefaultAPI,
		token:  token,
		http:   &http.Client{Timeout: 30 * time.Second},
		ua:     "DiscordBot (https://github.com/kronwerke/bot, " + version + ")",
		blocks: map[string]time.Time{},
	}
}

// route groups paths for rate limiting: ids in major parameters stay, other ids collapse.
func route(method, path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		if i > 0 && isSnowflake(parts[i]) {
			prev := parts[i-1]
			if prev == "channels" || prev == "guilds" || prev == "webhooks" {
				continue
			}
			parts[i] = ":id"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

func isSnowflake(s string) bool {
	if len(s) < 15 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *REST) wait(ctx context.Context, rt string) error {
	r.mu.Lock()
	until := r.blocks[rt]
	r.mu.Unlock()
	if d := time.Until(until); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *REST) block(rt string, d time.Duration) {
	r.mu.Lock()
	r.blocks[rt] = time.Now().Add(d)
	r.mu.Unlock()
}

// Do sends a JSON request. body and out may be nil. reason goes into the audit log.
func (r *REST) Do(ctx context.Context, method, path string, body, out any, reason string) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("discord: encode %s %s: %w", method, path, err)
		}
	}
	return r.send(ctx, method, path, "application/json", payload, out, reason)
}

// DoFiles sends a multipart request with payload_json and files.
func (r *REST) DoFiles(ctx context.Context, method, path string, body any, files []File, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pj, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("discord: encode %s %s: %w", method, path, err)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="payload_json"`)
	h.Set("Content-Type", "application/json")
	w, _ := mw.CreatePart(h)
	w.Write(pj)
	for i, f := range files {
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[%d]"; filename="%s"`, i, f.Name))
		ct := f.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		fh.Set("Content-Type", ct)
		fw, _ := mw.CreatePart(fh)
		fw.Write(f.Data)
	}
	mw.Close()
	return r.send(ctx, method, path, mw.FormDataContentType(), buf.Bytes(), out, "")
}

func (r *REST) send(ctx context.Context, method, path, ctype string, payload []byte, out any, reason string) error {
	rt := route(method, path)
	for attempt := 0; attempt < 4; attempt++ {
		if err := r.wait(ctx, rt); err != nil {
			return err
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, r.Base+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+r.token)
		req.Header.Set("User-Agent", r.ua)
		if payload != nil {
			req.Header.Set("Content-Type", ctype)
		}
		if reason != "" {
			req.Header.Set("X-Audit-Log-Reason", url.PathEscape(reason))
		}
		resp, err := r.http.Do(req)
		if err != nil {
			return fmt.Errorf("discord: %s %s: %w", method, path, err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if s, err := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Reset-After"), 64); err == nil {
				r.block(rt, time.Duration(s*float64(time.Second)))
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			json.Unmarshal(data, &rl)
			d := time.Duration(rl.RetryAfter*float64(time.Second)) + 50*time.Millisecond
			if d > time.Minute {
				d = time.Minute
			}
			r.block(rt, d)
			continue
		}
		if resp.StatusCode >= 500 && attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode/100 != 2 {
			ae := &APIError{Status: resp.StatusCode, Method: method, Path: path}
			json.Unmarshal(data, ae)
			if ae.Message == "" {
				ae.Message = strings.TrimSpace(string(data))
			}
			return ae
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("discord: decode %s %s: %w", method, path, err)
			}
		}
		return nil
	}
	return fmt.Errorf("discord: %s %s: still rate limited after retries", method, path)
}

// ---- helpers for the calls the bot makes ----

func (r *REST) Me(ctx context.Context) (User, error) {
	var u User
	err := r.Do(ctx, http.MethodGet, "/users/@me", nil, &u, "")
	return u, err
}

func (r *REST) GatewayURL(ctx context.Context) (string, error) {
	var g struct {
		URL string `json:"url"`
	}
	err := r.Do(ctx, http.MethodGet, "/gateway/bot", nil, &g, "")
	return g.URL, err
}

func (r *REST) SendMessage(ctx context.Context, channelID string, m MessageSend) (Message, error) {
	var out Message
	err := r.Do(ctx, http.MethodPost, "/channels/"+channelID+"/messages", m, &out, "")
	return out, err
}

func (r *REST) SendMessageFiles(ctx context.Context, channelID string, m MessageSend, files []File) (Message, error) {
	var out Message
	err := r.DoFiles(ctx, http.MethodPost, "/channels/"+channelID+"/messages", m, files, &out)
	return out, err
}

func (r *REST) EditMessage(ctx context.Context, channelID, messageID string, m MessageSend) (Message, error) {
	var out Message
	err := r.Do(ctx, http.MethodPatch, "/channels/"+channelID+"/messages/"+messageID, m, &out, "")
	return out, err
}

func (r *REST) DeleteMessage(ctx context.Context, channelID, messageID, reason string) error {
	return r.Do(ctx, http.MethodDelete, "/channels/"+channelID+"/messages/"+messageID, nil, nil, reason)
}

func (r *REST) Messages(ctx context.Context, channelID string, limit int) ([]Message, error) {
	var out []Message
	err := r.Do(ctx, http.MethodGet, fmt.Sprintf("/channels/%s/messages?limit=%d", channelID, limit), nil, &out, "")
	return out, err
}

func (r *REST) AddRole(ctx context.Context, guildID, userID, roleID, reason string) error {
	return r.Do(ctx, http.MethodPut, "/guilds/"+guildID+"/members/"+userID+"/roles/"+roleID, nil, nil, reason)
}

func (r *REST) RemoveRole(ctx context.Context, guildID, userID, roleID, reason string) error {
	return r.Do(ctx, http.MethodDelete, "/guilds/"+guildID+"/members/"+userID+"/roles/"+roleID, nil, nil, reason)
}

func (r *REST) Member(ctx context.Context, guildID, userID string) (Member, error) {
	var m Member
	err := r.Do(ctx, http.MethodGet, "/guilds/"+guildID+"/members/"+userID, nil, &m, "")
	return m, err
}

// Members lists every member of the guild, paging through 1000 at a time.
func (r *REST) Members(ctx context.Context, guildID string) ([]Member, error) {
	var all []Member
	after := "0"
	for {
		var page []Member
		if err := r.Do(ctx, http.MethodGet, fmt.Sprintf("/guilds/%s/members?limit=1000&after=%s", guildID, after), nil, &page, ""); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 1000 {
			return all, nil
		}
		after = page[len(page)-1].User.ID
	}
}

func (r *REST) Channels(ctx context.Context, guildID string) ([]Channel, error) {
	var out []Channel
	err := r.Do(ctx, http.MethodGet, "/guilds/"+guildID+"/channels", nil, &out, "")
	return out, err
}

func (r *REST) CreateChannel(ctx context.Context, guildID string, c Channel, reason string) (Channel, error) {
	var out Channel
	err := r.Do(ctx, http.MethodPost, "/guilds/"+guildID+"/channels", c, &out, reason)
	return out, err
}

func (r *REST) EditChannel(ctx context.Context, channelID string, patch map[string]any, reason string) (Channel, error) {
	var out Channel
	err := r.Do(ctx, http.MethodPatch, "/channels/"+channelID, patch, &out, reason)
	return out, err
}

func (r *REST) DeleteChannel(ctx context.Context, channelID, reason string) error {
	return r.Do(ctx, http.MethodDelete, "/channels/"+channelID, nil, nil, reason)
}

func (r *REST) CreateDM(ctx context.Context, userID string) (Channel, error) {
	var out Channel
	err := r.Do(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": userID}, &out, "")
	return out, err
}

// Respond answers an interaction.
func (r *REST) Respond(ctx context.Context, i *Interaction, callbackType int, data any) error {
	body := map[string]any{"type": callbackType}
	if data != nil {
		body["data"] = data
	}
	return r.Do(ctx, http.MethodPost, "/interactions/"+i.ID+"/"+i.Token+"/callback", body, nil, "")
}

// RespondFiles answers an interaction with attachments.
func (r *REST) RespondFiles(ctx context.Context, i *Interaction, callbackType int, data MessageSend, files []File) error {
	body := map[string]any{"type": callbackType, "data": data}
	return r.DoFiles(ctx, http.MethodPost, "/interactions/"+i.ID+"/"+i.Token+"/callback", body, files, nil)
}

// EditReply edits the original response of an interaction (after a deferred answer).
func (r *REST) EditReply(ctx context.Context, i *Interaction, m MessageSend) error {
	return r.Do(ctx, http.MethodPatch, "/webhooks/"+i.ApplicationID+"/"+i.Token+"/messages/@original", m, nil, "")
}

// SetGuildCommands replaces every slash command of the application in a guild.
func (r *REST) SetGuildCommands(ctx context.Context, appID, guildID string, cmds []ApplicationCommand) error {
	return r.Do(ctx, http.MethodPut, "/applications/"+appID+"/guilds/"+guildID+"/commands", cmds, nil, "")
}
