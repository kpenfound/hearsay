package distiller

import "github.com/kpenfound/hearsay/internal/l1"

// DecideOutcome is decideOutcome, for the package's external tests.
var DecideOutcome = decideOutcome

// OwnerOutcome is ownerOutcome, for the package's external tests.
func OwnerOutcome(doc l1.Document, summary, outcome string, cited []int) l1.Body {
	return ownerOutcome(doc, answer{Summary: summary, Outcome: outcome, OutcomeLines: cited})
}
