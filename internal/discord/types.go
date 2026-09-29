// Package discord talks to the Discord API: REST with rate limits, and the gateway
// with heartbeats, resume and reconnect. Only what the Kronwerke bot uses.
package discord

import "encoding/json"

// Gateway intents the bot needs.
const (
	IntentGuilds         = 1 << 0
	IntentGuildMembers   = 1 << 1
	IntentGuildMessages  = 1 << 9
	IntentMessageContent = 1 << 15
)

// Permission bits (strings in the API).
const (
	PermViewChannel        = 1 << 10
	PermSendMessages       = 1 << 11
	PermAttachFiles        = 1 << 15
	PermReadMessageHistory = 1 << 16
)

// Interaction types.
const (
	InteractionPing         = 1
	InteractionCommand      = 2
	InteractionComponent    = 3
	InteractionAutocomplete = 4
	InteractionModalSubmit  = 5
)

// Interaction callback types.
const (
	CallbackMessage         = 4
	CallbackDeferredMessage = 5
	CallbackDeferredUpdate  = 6
	CallbackUpdateMessage   = 7
	CallbackModal           = 9
)

// Message flags.
const FlagEphemeral = 1 << 6

type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name,omitempty"`
	Bot        bool   `json:"bot,omitempty"`
}

// Name is what a person sees.
func (u User) Name() string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

type Member struct {
	User     *User    `json:"user,omitempty"`
	Nick     string   `json:"nick,omitempty"`
	Roles    []string `json:"roles"`
	JoinedAt string   `json:"joined_at,omitempty"`
	Pending  bool     `json:"pending,omitempty"`
	GuildID  string   `json:"guild_id,omitempty"`
}

// HasRole reports whether the member has any of the roles.
func (m *Member) HasRole(ids ...string) bool {
	if m == nil {
		return false
	}
	for _, r := range m.Roles {
		for _, id := range ids {
			if id != "" && r == id {
				return true
			}
		}
	}
	return false
}

type Message struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	GuildID   string  `json:"guild_id,omitempty"`
	Author    User    `json:"author"`
	Member    *Member `json:"member,omitempty"`
	Content   string  `json:"content"`
	Timestamp string  `json:"timestamp"`
}

type Channel struct {
	ID                   string      `json:"id"`
	Type                 int         `json:"type"`
	GuildID              string      `json:"guild_id,omitempty"`
	Name                 string      `json:"name"`
	ParentID             string      `json:"parent_id,omitempty"`
	Topic                string      `json:"topic,omitempty"`
	PermissionOverwrites []Overwrite `json:"permission_overwrites,omitempty"`
}

type Overwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"` // 0 role, 1 member
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

type Role struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
}

// Interaction is an application command, a component click or a modal submit.
type Interaction struct {
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	Type          int             `json:"type"`
	Token         string          `json:"token"`
	GuildID       string          `json:"guild_id,omitempty"`
	ChannelID     string          `json:"channel_id,omitempty"`
	Member        *Member         `json:"member,omitempty"`
	User          *User           `json:"user,omitempty"`
	Data          InteractionData `json:"data"`
	Message       *Message        `json:"message,omitempty"`
}

// Actor is the user behind an interaction, in a guild or in DMs.
func (i *Interaction) Actor() User {
	if i.Member != nil && i.Member.User != nil {
		return *i.Member.User
	}
	if i.User != nil {
		return *i.User
	}
	return User{}
}

type InteractionData struct {
	Name          string          `json:"name,omitempty"`
	CustomID      string          `json:"custom_id,omitempty"`
	ComponentType int             `json:"component_type,omitempty"`
	Options       []CommandOption `json:"options,omitempty"`
	Components    []Component     `json:"components,omitempty"`
	Resolved      *ResolvedData   `json:"resolved,omitempty"`
}

type ResolvedData struct {
	Users   map[string]User   `json:"users,omitempty"`
	Members map[string]Member `json:"members,omitempty"`
}

type CommandOption struct {
	Name    string          `json:"name"`
	Type    int             `json:"type"`
	Value   json.RawMessage `json:"value,omitempty"`
	Options []CommandOption `json:"options,omitempty"`
}

// String returns a string option value, or "".
func (o CommandOption) String() string {
	var s string
	json.Unmarshal(o.Value, &s)
	return s
}

// Option finds a top level option by name.
func (d InteractionData) Option(name string) (CommandOption, bool) {
	for _, o := range d.Options {
		if o.Name == name {
			return o, true
		}
	}
	return CommandOption{}, false
}

// ModalValue finds a text input value in a modal submit.
func (d InteractionData) ModalValue(customID string) string {
	for _, row := range d.Components {
		for _, c := range row.Components {
			if c.CustomID == customID {
				return c.Value
			}
		}
	}
	return ""
}

// Component is used both for sending (buttons, text inputs) and for reading modal values.
type Component struct {
	Type        int         `json:"type"`
	CustomID    string      `json:"custom_id,omitempty"`
	Label       string      `json:"label,omitempty"`
	Style       int         `json:"style,omitempty"`
	Emoji       *Emoji      `json:"emoji,omitempty"`
	URL         string      `json:"url,omitempty"`
	Disabled    bool        `json:"disabled,omitempty"`
	Placeholder string      `json:"placeholder,omitempty"`
	MinLength   int         `json:"min_length,omitempty"`
	MaxLength   int         `json:"max_length,omitempty"`
	Required    *bool       `json:"required,omitempty"`
	Value       string      `json:"value,omitempty"`
	Components  []Component `json:"components,omitempty"`
}

type Emoji struct {
	Name string `json:"name"`
}

// Component types and styles.
const (
	ComponentActionRow = 1
	ComponentButton    = 2
	ComponentTextInput = 4

	ButtonPrimary   = 1
	ButtonSecondary = 2
	ButtonSuccess   = 3
	ButtonDanger    = 4
	ButtonLink      = 5

	TextShort     = 1
	TextParagraph = 2
)

// Row wraps components in an action row.
func Row(cs ...Component) Component { return Component{Type: ComponentActionRow, Components: cs} }

// Button is an interactive button.
func Button(customID, label string, style int) Component {
	return Component{Type: ComponentButton, CustomID: customID, Label: label, Style: style}
}

// TextInput is a text input for a modal.
func TextInput(customID, label string, style, minLen, maxLen int, required bool, placeholder string) Component {
	r := required
	return Component{Type: ComponentTextInput, CustomID: customID, Label: label, Style: style,
		MinLength: minLen, MaxLength: maxLen, Required: &r, Placeholder: placeholder}
}

type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Image       *EmbedImage  `json:"image,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type EmbedImage struct {
	URL string `json:"url"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

// MessageSend is the body for creating or editing a message and for interaction replies.
type MessageSend struct {
	Content         string           `json:"content"`
	Embeds          []Embed          `json:"embeds,omitempty"`
	Components      []Component      `json:"components"`
	Flags           int              `json:"flags,omitempty"`
	Attachments     []Attachment     `json:"attachments,omitempty"`
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

type Attachment struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
}

type AllowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
	Users []string `json:"users,omitempty"`
}

// NoMentions keeps a message from pinging anyone.
var NoMentions = &AllowedMentions{Parse: []string{}}

// File is an upload for a multipart request.
type File struct {
	Name        string
	ContentType string
	Data        []byte
}

// ApplicationCommand is a slash command definition.
type ApplicationCommand struct {
	Name                     string                     `json:"name"`
	Description              string                     `json:"description"`
	Type                     int                        `json:"type,omitempty"`
	Options                  []ApplicationCommandOption `json:"options,omitempty"`
	DefaultMemberPermissions *string                    `json:"default_member_permissions,omitempty"`
	DMPermission             *bool                      `json:"dm_permission,omitempty"`
}

type ApplicationCommandOption struct {
	Type        int                        `json:"type"`
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	Required    bool                       `json:"required,omitempty"`
	MinLength   int                        `json:"min_length,omitempty"`
	MaxLength   int                        `json:"max_length,omitempty"`
	Options     []ApplicationCommandOption `json:"options,omitempty"`
}

// Option types.
const (
	OptSubCommand = 1
	OptString     = 3
	OptInteger    = 4
	OptUser       = 6
)
