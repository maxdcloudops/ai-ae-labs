package main

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
)

// Role is who the run is acting for. It comes from the CLI (and would come
// from the authenticated caller in a real service), never from the model.
type Role string

// The three roles of this lab.
const (
	// RoleCoordinator may ask every desk.
	RoleCoordinator Role = "coordinator"
	// RoleAnalyst may read risk and compliance, but not merchant data.
	RoleAnalyst Role = "analyst"
	// RoleGuest may read nothing. A run as guest produces a document of gaps.
	RoleGuest Role = "guest"
)

// policyDeniedCode is the section code for a refusal. It is deliberately
// distinct from a source outage: "you may not ask" and "it did not answer"
// need different responses from whoever reads the report.
const policyDeniedCode = "POLICY_DENIED"

// allowedTools is the authorization table: role → tools it may call.
//
// This is the shape OPA or Cedar would hold as a policy document. Keeping it
// as data rather than as `if` statements is the whole point: a reviewer can
// read who may do what without reading the agent.
var allowedTools = map[Role]map[string]bool{
	RoleCoordinator: {
		"lookup_risk_desk":        true,
		"lookup_compliance_desk":  true,
		"lookup_integration_desk": true,
	},
	RoleAnalyst: {
		"lookup_risk_desk":       true,
		"lookup_compliance_desk": true,
		// lookup_integration_desk is absent: it returns merchant identifiers,
		// and an analyst has no business reading them for a board summary.
	},
	RoleGuest: {},
}

// may reports whether role may call toolName. An unknown role may do nothing:
// fail-closed, because the alternative is that a typo in a config grants
// everything.
func may(role Role, toolName string) bool {
	return allowedTools[role][toolName]
}

// policyGate is the BeforeToolCallback that blocks a forbidden call.
//
// Returning a non-nil map short-circuits the tool: it never runs, and the map
// becomes the result the model sees. So the model learns it was refused, with
// the reason, instead of silently receiving nothing.
//
// IMPORTANT, and this is the part worth arguing about: this callback is NOT
// the authorization boundary. It only covers the path through the model. In
// this graph the branch node also reads the source directly (that is how the
// evidence stays out of the model's hands), and that path does not pass
// through any callback. A policy enforced only here would be theatre: the
// document would still contain the data the role may not see.
//
// The boundary is `may` itself, called at BOTH places — see Engine.branch.
func policyGate(role Role) llmagent.BeforeToolCallback {
	return func(_ agent.Context, t tool.Tool, _ map[string]any) (map[string]any, error) {
		if may(role, t.Name()) {
			return nil, nil
		}
		return map[string]any{
			"status": "denied",
			"code":   policyDeniedCode,
			"reason": fmt.Sprintf("роль %q не має права викликати %s", role, t.Name()),
		}, nil
	}
}

// policyError is what the branch gets when the role may not read a source. It
// wraps the same sentinel as a source outage so the containment path is
// identical — but it carries its own code, so the report says WHY the section
// is empty.
func policyError(role Role, toolName string) error {
	return &deskError{
		Code: policyDeniedCode,
		Why:  fmt.Sprintf("роль %q не має доступу до %s", role, toolName),
	}
}
