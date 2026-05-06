// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/lang/langrefs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type actionTriggerApplyInstance struct {
	ActionInvocation *plans.ActionInvocationInstanceSrc
	resolvedProvider addrs.AbsProviderConfig

	// FIXME: this is no longer populated
	ActionTriggerRange *hcl.Range

	ConditionExpr hcl.Expression

	// link the trigger to it's action config
	// this is connected by the diff transformer
	actionNode *NodeActionConfig
}

var (
	_ GraphNodeExecutable       = (*actionTriggerApplyInstance)(nil)
	_ GraphNodeReferencer       = (*actionTriggerApplyInstance)(nil)
	_ GraphNodeProviderConsumer = (*actionTriggerApplyInstance)(nil)
	_ GraphNodeModulePath       = (*actionTriggerApplyInstance)(nil)
)

func (n *actionTriggerApplyInstance) Name() string {
	return n.ActionInvocation.Addr.String() + " (instance)"
}

func (n *actionTriggerApplyInstance) Execute(ctx EvalContext, wo walkOperation) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	actionInvocation := n.ActionInvocation

	if n.ConditionExpr != nil {
		// We know this must be a lifecycle action, otherwise we would have no condition
		at := actionInvocation.ActionTrigger.(*plans.ResourceActionTrigger)
		condition, conditionDiags := evaluateActionCondition(ctx, actionConditionContext{
			// For applying the triggering event is sufficient, if the condition could not have
			// been evaluated due to in invalid mix of events we would have caught it durin planning.
			events:          []configs.ActionTriggerEvent{at.ActionTriggerEvent},
			conditionExpr:   n.ConditionExpr,
			resourceAddress: at.TriggeringResourceAddr,
		})
		diags = diags.Append(conditionDiags)
		if diags.HasErrors() {
			return diags
		}

		if !condition {
			return diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Condition changed evaluation during apply",
				Detail:   "The condition evaluated to false during apply, but was true during planning. This may lead to unexpected behavior.",
				Subject:  n.ConditionExpr.Range().Ptr(),
			})
		}
	}

	provider, _, err := getProvider(ctx, n.resolvedProvider)
	if err != nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Failed to get provider for %s", n.resolvedProvider),
			Detail:   fmt.Sprintf("Failed to get provider: %s", err),
			Subject:  n.ActionTriggerRange,
		})
		return diags
	}

	if n.actionNode == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Invoke %s missing action config", n.ActionInvocation.Addr),
			Detail:   fmt.Sprintf("The action config was not found for invocation %s", n.ActionInvocation.Addr),
			Subject:  n.ActionTriggerRange,
		})
		return diags
	}

	configValue, actionDiags := n.actionNode.Eval(ctx)
	diags = diags.Append(actionDiags)
	if diags.HasErrors() {
		return diags
	}

	// FIXME: action eval is going to be refactored so we don't need to duplicate this
	switch key := n.ActionInvocation.Addr.Action.Key.(type) {
	case addrs.StringKey:
		switch {
		case configValue.Type().IsMapType():
			configValue = configValue.Index(key.Value())
		case configValue.Type().IsObjectType():
			configValue = configValue.GetAttr(key.Value().AsString())
		default:
			panic(fmt.Sprintf("invalid config value type: %#v", configValue.Type()))
		}
	case addrs.IntKey:
		configValue = configValue.Index(key.Value())
	}

	if !configValue.IsWhollyKnown() {
		return diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Action configuration unknown during apply",
			Detail:   fmt.Sprintf("The action %s was not fully known during apply.\n\nThis is a bug in Terraform, please report it.", n.ActionInvocation.Addr),
			// FIXME: maybe turn this into an attribute path diagnostic?
			Subject: n.actionNode.Config.DeclRange.Ptr(),
		})
	}

	hookIdentity := HookActionIdentity{
		Addr:          n.ActionInvocation.Addr,
		ActionTrigger: n.ActionInvocation.ActionTrigger,
	}

	diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
		return h.StartAction(hookIdentity)
	}))
	if diags.HasErrors() {
		return diags
	}

	// We don't want to send the marks, but all marks are okay in the context
	// of an action invocation. We can't reuse our ephemeral free value from
	// above because we want the ephemeral values to be included.
	unmarkedConfigValue, _ := configValue.UnmarkDeep()
	resp := provider.InvokeAction(providers.InvokeActionRequest{
		ActionType:         n.ActionInvocation.Addr.Action.Action.Type,
		PlannedActionData:  unmarkedConfigValue,
		ClientCapabilities: ctx.ClientCapabilities(),
	})

	respDiags := n.AddSubjectToDiagnostics(resp.Diagnostics)
	diags = diags.Append(respDiags)
	if respDiags.HasErrors() {
		diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
			return h.CompleteAction(hookIdentity, respDiags.Err())
		}))
		return diags
	}

	if resp.Events != nil { // should only occur in misconfigured tests
		for event := range resp.Events {
			switch ev := event.(type) {
			case providers.InvokeActionEvent_Progress:
				diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
					return h.ProgressAction(hookIdentity, ev.Message)
				}))
				if diags.HasErrors() {
					return diags
				}
			case providers.InvokeActionEvent_Completed:
				// Enhance the diagnostics
				diags = diags.Append(n.AddSubjectToDiagnostics(ev.Diagnostics))
				diags = diags.Append(ctx.Hook(func(h Hook) (HookAction, error) {
					return h.CompleteAction(hookIdentity, ev.Diagnostics.Err())
				}))
				if ev.Diagnostics.HasErrors() {
					return diags
				}
				if diags.HasErrors() {
					return diags
				}
			default:
				panic(fmt.Sprintf("unexpected action event type %T", ev))
			}
		}
	} else {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Provider return invalid response",
			Detail:   "Provider response did not include any events",
			Subject:  n.ActionTriggerRange,
		})
	}

	return diags
}

func (n *actionTriggerApplyInstance) Provider() ProviderRef {
	return ProviderRef{
		Addr:     n.ActionInvocation.ProviderAddr,
		Resolved: true,
	}
}

func (n *actionTriggerApplyInstance) SetProvider(config addrs.AbsProviderConfig) {
	n.resolvedProvider = config
}

func (n *actionTriggerApplyInstance) References() []*addrs.Reference {
	var refs []*addrs.Reference

	refs = append(refs, &addrs.Reference{
		Subject: n.ActionInvocation.Addr.Action,
	})

	conditionRefs, refDiags := langrefs.ReferencesInExpr(addrs.ParseRef, n.ConditionExpr)
	if refDiags.HasErrors() {
		panic(fmt.Sprintf("error parsing references in expression: %v", refDiags))
	}
	if conditionRefs != nil {
		refs = append(refs, conditionRefs...)
	}

	return refs
}

// GraphNodeReferencer
func (n *actionTriggerApplyInstance) ModulePath() addrs.Module {
	return n.ActionInvocation.Addr.Module.Module()
}

// GraphNodeExecutable
func (n *actionTriggerApplyInstance) Path() addrs.ModuleInstance {
	return n.ActionInvocation.Addr.Module
}

func (n *actionTriggerApplyInstance) AddSubjectToDiagnostics(input tfdiags.Diagnostics) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if len(input) > 0 {
		severity := hcl.DiagWarning
		message := "Warning when invoking action"
		err := input.Warnings().ErrWithWarnings()
		if input.HasErrors() {
			severity = hcl.DiagError
			message = "Error when invoking action"
			err = input.ErrWithWarnings()
		}

		diags = diags.Append(&hcl.Diagnostic{
			Severity: severity,
			Summary:  message,
			Detail:   err.Error(),

			// FIXME: this is the action config block, make sure user can associate this with the trigger
			Subject: n.actionNode.Config.DeclRange.Ptr(),
		})
	}
	return diags
}

type actionConditionContext struct {
	events          []configs.ActionTriggerEvent
	conditionExpr   hcl.Expression
	resourceAddress addrs.AbsResourceInstance
}

func evaluateActionCondition(ctx EvalContext, at actionConditionContext) (bool, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	rd := instances.RepetitionData{}
	refs, refDiags := langrefs.ReferencesInExpr(addrs.ParseRef, at.conditionExpr)
	diags = diags.Append(refDiags)
	if diags.HasErrors() {
		return false, diags
	}

	for _, ref := range refs {
		if ref.Subject == addrs.Self {
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Self reference not allowed",
				Detail:   `The condition expression cannot reference "self".`,
				Subject:  at.conditionExpr.Range().Ptr(),
			})
		}
	}

	if diags.HasErrors() {
		return false, diags
	}

	if containsBeforeEvent(at.events) {
		// If events contains a before event we want to error if count or each is used
		for _, ref := range refs {
			if _, ok := ref.Subject.(addrs.CountAttr); ok {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Count reference not allowed",
					Detail:   `The condition expression cannot reference "count" if the action is run before the resource is applied.`,
					Subject:  at.conditionExpr.Range().Ptr(),
				})
			}

			if _, ok := ref.Subject.(addrs.ForEachAttr); ok {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Each reference not allowed",
					Detail:   `The condition expression cannot reference "each" if the action is run before the resource is applied.`,
					Subject:  at.conditionExpr.Range().Ptr(),
				})
			}

			if diags.HasErrors() {
				return false, diags
			}
		}
	} else {
		// If there are only after events we allow self, count, and each
		expander := ctx.InstanceExpander()
		rd = expander.GetResourceInstanceRepetitionData(at.resourceAddress)
	}

	scope := ctx.EvaluationScope(nil, nil, rd)
	val, conditionEvalDiags := scope.EvalExpr(at.conditionExpr, cty.Bool)
	diags = diags.Append(conditionEvalDiags)
	if diags.HasErrors() {
		return false, diags
	}

	if !val.IsWhollyKnown() {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Condition must be known",
			Detail:   "The condition expression resulted in an unknown value, but it must be a known boolean value.",
			Subject:  at.conditionExpr.Range().Ptr(),
		})
		return false, diags
	}

	return val.True(), nil
}

func containsBeforeEvent(events []configs.ActionTriggerEvent) bool {
	for _, event := range events {
		switch event {
		case configs.EventBeforeCreate, configs.EventBeforeUpdate:
			return true
		default:
			continue
		}
	}
	return false
}
