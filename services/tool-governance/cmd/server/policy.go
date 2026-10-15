package main

import (
	"fmt"
	"net/http"
	"strings"

	executionadapters "github.com/dylmarriner/scos-memory/packages/execution-adapters"
)

// OperationPlan is the plan a caller submits with an operation. Its fields
// mirror execution-proxy's CommandPlan so the proxy's plan is persisted as
// sent; the command fields are also re-graded by the shared risk
// classifier, so a caller cannot understate the risk of what it will run.
type OperationPlan struct {
	OriginalRequest  string `json:"original_request,omitempty"`
	ExecutedRequest  string `json:"executed_request,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
	RewriteSummary   string `json:"rewrite_summary,omitempty"`
	RiskLevel        string `json:"risk_level,omitempty"`
	Mode             string `json:"mode,omitempty"`
	PassThrough      bool   `json:"pass_through,omitempty"`
	DryRun           bool   `json:"dry_run,omitempty"`
}

// riskRank orders the shared risk levels (packages/execution-adapters) from
// least to most dangerous. A level missing from this map is not a risk
// level governance knows how to decide on.
var riskRank = map[string]int{
	executionadapters.RiskRead:        0,
	executionadapters.RiskTest:        1,
	executionadapters.RiskWrite:       2,
	executionadapters.RiskDestructive: 3,
}

// operationRequest is everything a caller asserts about one operation that
// the governance decision depends on.
type operationRequest struct {
	Risk        string
	Plan        OperationPlan
	Confirmed   bool
	OperationID string
}

// operationDecision records what governance decided for an accepted
// operation; it is persisted with the operation's tool_executions row.
type operationDecision struct {
	RequestedRisk string
	EffectiveRisk string
	Plan          OperationPlan
	Confirmed     bool
}

// policyRefusal is a governance decision not to authorise an operation.
type policyRefusal struct {
	Status  int
	Message string
}

func (r *policyRefusal) Error() string { return r.Message }

// requestedRisk resolves the risk the caller declared, from the top-level
// risk field or the plan's risk_level. Declaring two different levels is a
// malformed request rather than something to silently pick between.
func requestedRisk(risk string, plan OperationPlan) (string, error) {
	top := strings.ToLower(strings.TrimSpace(risk))
	inPlan := strings.ToLower(strings.TrimSpace(plan.RiskLevel))
	if top != "" && inPlan != "" && top != inPlan {
		return "", fmt.Errorf("risk %q does not match plan.risk_level %q", top, inPlan)
	}
	declared := top
	if declared == "" {
		declared = inPlan
	}
	if declared != "" {
		if _, ok := riskRank[declared]; !ok {
			return "", fmt.Errorf("risk must be one of read, test, write, destructive; got %q", declared)
		}
	}
	return declared, nil
}

// effectiveRisk is the risk governance decides on: the highest of the
// declared risk and the shared classifier's grade of every command the plan
// names. An operation that declares no risk and plans no command gives
// governance nothing to decide on and is refused.
func effectiveRisk(declared string, plan OperationPlan) (string, error) {
	effective := declared
	for _, command := range []string{plan.OriginalRequest, plan.ExecutedRequest} {
		if strings.TrimSpace(command) == "" {
			continue
		}
		graded := executionadapters.ClassifyRisk(command)
		if effective == "" || riskRank[graded] > riskRank[effective] {
			effective = graded
		}
	}
	if effective == "" {
		return "", fmt.Errorf("risk is required: declare risk (read, test, write, destructive) or submit a plan naming the command")
	}
	return effective, nil
}

// decideOperation is the single authorisation decision for an operation on
// a registered tool, used by both /v1/operations/prepare and
// /v1/operations/start. The tool must be authorised; the effective risk
// must fit the tool's declared properties; and destructive work, or any
// work on a confirmation-gated tool, must carry explicit confirmation.
func decideOperation(tool *ToolDefinition, req operationRequest) (operationDecision, *policyRefusal) {
	if tool.State != StateAuthorised {
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is %s, not authorised", tool.Name, tool.State)}
	}
	declared, err := requestedRisk(req.Risk, req.Plan)
	if err != nil {
		return operationDecision{}, &policyRefusal{http.StatusBadRequest, err.Error()}
	}
	effective, err := effectiveRisk(declared, req.Plan)
	if err != nil {
		return operationDecision{}, &policyRefusal{http.StatusBadRequest, err.Error()}
	}
	props := tool.Props
	switch {
	case props.ReadOnly && effective != executionadapters.RiskRead:
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is declared read_only; a %s operation is not permitted", tool.Name, effective)}
	case !props.Destructive && effective == executionadapters.RiskDestructive:
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is not declared destructive; a destructive operation is not permitted", tool.Name)}
	case !props.ExternallySideEffecting && (effective == executionadapters.RiskWrite || effective == executionadapters.RiskDestructive):
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is not declared externally_side_effecting; a %s operation is not permitted", tool.Name, effective)}
	case req.Plan.DryRun && !props.DryRunCapable:
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is not declared dry_run_capable", tool.Name)}
	case props.ConfirmationGated && !req.Confirmed:
		return operationDecision{}, &policyRefusal{http.StatusForbidden, fmt.Sprintf("tool %q is confirmation_gated; explicit confirmation is required", tool.Name)}
	case effective == executionadapters.RiskDestructive && !req.Confirmed:
		return operationDecision{}, &policyRefusal{http.StatusForbidden, "explicit confirmation is required for a destructive operation"}
	case !props.Idempotent && effective != executionadapters.RiskRead && strings.TrimSpace(req.OperationID) == "":
		// A retry of a non-idempotent side effect must name the operation it
		// retries, so it replays that operation instead of running again.
		return operationDecision{}, &policyRefusal{http.StatusBadRequest, fmt.Sprintf("tool %q is not idempotent; operation_id is required for a %s operation", tool.Name, effective)}
	}
	plan := req.Plan
	plan.RiskLevel = declared
	return operationDecision{RequestedRisk: declared, EffectiveRisk: effective, Plan: plan, Confirmed: req.Confirmed}, nil
}
