// Package push holds the mechanics shared by bearer-authenticated JSON push
// connectors: token loading and matching, bounded single-object requests and
// HTTP status mapping, content-derived revisions and artifact retractions.
// Connectors own request shapes, authorization policy, event mapping and the
// work that must finish before a request is accepted. This package owns no
// source-specific policy, storage or goroutines.
package push
