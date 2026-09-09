// Package principal is the identity model: one person, one agent or one bot,
// across every source they appear in.
//
// A Slack user id, a GitHub login, an email address and a calendar attendee are
// the same principal. Without that mapping a stance has no consistent author
// and authority cannot be checked, so this package is load-bearing for access
// control rather than a convenience.
//
// See docs/design.md#configuration and docs/design.md#access-control.
package principal
