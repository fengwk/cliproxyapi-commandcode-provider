package provider

import "strings"

// planCreditsSnapshotDate is the date the official plan-tier baseline table
// below was read from CommandCode's public, unauthenticated web assets
// (commandcode.ai/assets/plan-tiers-DIOqpikH.js). It is a dated snapshot of
// published plan facts, never a live account cap.
const planCreditsSnapshotDate = "2026-10-14"

// planTeamsPro is the only seat-based plan captured in the snapshot table.
const planTeamsPro = "teams-pro"

// officialPlanMonthlyCredits maps the official plan ids observed on the
// snapshot date to their published monthly totalCredits baseline. These values
// come from the public plan-tiers asset, not from any account API. The reviewed
// meter uses them as the grant's minimum or as fallback when no grant is usable.
var officialPlanMonthlyCredits = map[string]float64{
	"individual-go":       10,
	"individual-go-v1":    10,
	"individual-goat":     70,
	"individual-pro":      30,
	"individual-pro-v1":   80,
	"individual-provider": 15,
	"individual-max":      150,
	"individual-ultra":    300,
	"teams-pro":           40, // per seat
}

// normalizePlanID lowercases and trims a reported plan id and folds the limited
// underscore/hyphen separator variants together. Matching is always exact, so
// an unknown plan can never be misread as a known one through a prefix.
func normalizePlanID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
}

// officialPlanBase returns the dated published baseline for a known plan id.
// An empty or unknown plan yields false.
func officialPlanBase(facts *subscriptionFacts) (float64, bool) {
	if facts == nil || facts.PlanID == "" {
		return 0, false
	}
	base, ok := officialPlanMonthlyCredits[facts.PlanID]
	return base, ok
}
