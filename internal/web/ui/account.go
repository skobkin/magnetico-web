package ui

// Page data of the account, onboarding and recovery pages (W6). Every string is
// escaped by the templates; none of these types carries a secret except the
// one-time Secret of the account page and the invitation URLs, both shown only
// to their owner on no-store, no-referrer responses.

// RegisterPage is the invitation registration form.
type RegisterPage struct {
	CSRFToken string
	// Valid is false for every unusable invitation (one generic message).
	Valid    bool
	Username string
	Email    string
	Error    string
	// EmailHint is true when SMTP is configured and a verification email will
	// be offered for an address.
	EmailHint bool
}

// ForgotPage is the forgot-password form.
type ForgotPage struct {
	CSRFToken string
	// Available is false when SMTP is not configured.
	Available bool
	Sent      bool
	Email     string
}

// PasswordLinkPage is the setup/reset form behind a one-time link.
type PasswordLinkPage struct {
	CSRFToken string
	Setup     bool
	Error     string
}

// VerifyPage asks the holder of a verification link to confirm.
type VerifyPage struct {
	CSRFToken string
}

// TokenRow is one listed token. The secret is never part of it.
type TokenRow struct {
	ID       int64
	Label    string
	Created  string
	LastUsed string // "" = never
	Expires  string // "" = never
	Expired  bool
	Legacy   bool
}

// InviteRow is one invitation owned by the signed-in user.
type InviteRow struct {
	ID      int64
	URL     string // absolute registration link; "" unless Usable
	Status  string // "Available", "Used", "Revoked", "Expired"
	UsedBy  string
	Created string
	Usable  bool
}

// NewSecret is a credential shown exactly once, right after creation.
type NewSecret struct {
	Kind  string // "rss" or "api"
	Label string
	Token string
	URL   string // ready-to-copy RSS URL (rss only)
}

// AccountPage is the signed-in account page.
type AccountPage struct {
	Username  string
	CSRFToken string

	Tab    string // profile, invitations, rss, api
	Notice string // fixed success text
	Error  string // fixed failure text

	Role          string
	IsAdmin       bool // shows the Admin link
	Email         string
	EmailVerified bool
	PendingEmail  string
	MailEnabled   bool
	// EmailState is the state of the stored address: "none" (no address, no
	// pending one), "pending" (no address yet, one awaiting confirmation),
	// "unverified" or "verified". It is independent of MailEnabled (the
	// service state: whether this server can send email at all).
	EmailState string
	// RecoveryAvailable is true only when email recovery can work right now:
	// the stored address is verified and the server can send email. A pending
	// replacement does not change it.
	RecoveryAvailable bool
	// RSSV1Disabled: the deprecated v1 feed is switched off on this server
	// (MAGNETICO_WEB_RSS_V1_ENABLED=false); the RSS tab then says so instead of
	// telling users that the old address still works.
	RSSV1Disabled bool

	Invites          []InviteRow
	InvitesAvailable int
	RSSTokens        []TokenRow
	APITokens        []TokenRow
	Secret           *NewSecret
}

// DocsPage is a public API documentation page.
type DocsPage struct {
	Title   string
	SpecURL string // same-origin path of the OpenAPI document
	Version string // "v1" or "v2": the switcher marks it as current
}
