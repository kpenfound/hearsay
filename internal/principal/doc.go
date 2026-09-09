// Package principal is the identity model: one person, one agent or one team,
// across every source they appear in.
//
// A Discord user id, a GitHub login, an email address and a calendar attendee
// are the same principal. Without that mapping a stance has no consistent
// author and authority cannot be checked, so this package is load-bearing for
// access control rather than a convenience.
//
// The mapping is configuration's `principals/` (docs/config.md), parsed into
// [Principal] values. [Resolver] turns the identity hints a connector emits
// into principals against it, and records what it could not resolve rather than
// dropping an author nobody can name. [Class] is what an agent may read and
// write, and [AgentRead] is the effective principal a read runs as: the
// intersection of an agent's grants and the invoking human's
// (docs/design.md#access-control).
package principal
