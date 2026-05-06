// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/lang/langrefs"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type nodeActionTriggerApplyInstance struct {
	ActionInvocation *plans.ActionInvocationInstanceSrc
	resolvedProvider addrs.AbsProviderConfig

	// FIXME: this is no longer populated
	ActionTriggerRange *hcl.Range

	ConditionExpr hcl.Expression

	// link the trigger to it's action config
	// this is connected by the diff transformer
	actionConfig *NodeActionConfig
}

var (
	_ GraphNodeExecutable       = (*nodeActionTriggerApplyInstance)(nil)
	_ GraphNodeReferencer       = (*nodeActionTriggerApplyInstance)(nil)
	_ GraphNodeProviderConsumer = (*nodeActionTriggerApplyInstance)(nil)
	_ GraphNodeModulePath       = (*nodeActionTriggerApplyInstance)(nil)
)

func (n *nodeActionTriggerApplyInstance) Name() string {
	return n.ActionInvocation.Addr.String() + " (instance)"
}

func (n *nodeActionTriggerApplyInstance) Execute(ctx EvalContext, wo walkOperation) tfdiags.Diagnostics {
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

	if n.actionConfig == nil {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("Invoke %s missing action config", n.ActionInvocation.Addr),
			Detail:   fmt.Sprintf("The action config was not found for invocation %s", n.ActionInvocation.Addr),
			Subject:  n.ActionTriggerRange,
		})
		return diags
	}

	configValue, actionDiags := n.actionConfig.Eval(ctx)
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

	// FIXME: action plans can't alter the config value, so there's no reason to check with objchange
	//
	// // Validate that what we planned matches the action data we have.
	// errs := objchange.AssertObjectCompatible(actionSchema.ConfigSchema, ai.ConfigValue, ephemeral.RemoveEphemeralValues(configValue))
	// for _, err := range errs {
	// 	diags = diags.Append(&hcl.Diagnostic{
	// 		Severity: hcl.DiagError,
	// 		Summary:  "Provider produced inconsistent final plan",
	// 		Detail: fmt.Sprintf("When expanding the plan for %s to include new values learned so far during apply, Terraform produced an invalid new value for %s.\n\nThis is a bug in Terraform, which should be reported.",
	// 			ai.Addr, tfdiags.FormatError(err)),
	// 		Subject: n.ActionTriggerRange,
	// 	})
	// }

	if !configValue.IsWhollyKnown() {
		return diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Action configuration unknown during apply",
			Detail:   fmt.Sprintf("The action %s was not fully known during apply.\n\nThis is a bug in Terraform, please report it.", n.ActionInvocation.Addr),
			// FIXME: maybe turn this into an attribute path diagnostic?
			Subject: n.actionConfig.Config.DeclRange.Ptr(),
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

func (n *nodeActionTriggerApplyInstance) Provider() ProviderRef {
	return ProviderRef{
		Addr:     n.ActionInvocation.ProviderAddr,
		Resolved: true,
	}
}

func (n *nodeActionTriggerApplyInstance) SetProvider(config addrs.AbsProviderConfig) {
	n.resolvedProvider = config
}

func (n *nodeActionTriggerApplyInstance) References() []*addrs.Reference {
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
func (n *nodeActionTriggerApplyInstance) ModulePath() addrs.Module {
	return n.ActionInvocation.Addr.Module.Module()
}

// GraphNodeExecutable
func (n *nodeActionTriggerApplyInstance) Path() addrs.ModuleInstance {
	return n.ActionInvocation.Addr.Module
}

func (n *nodeActionTriggerApplyInstance) AddSubjectToDiagnostics(input tfdiags.Diagnostics) tfdiags.Diagnostics {
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
			Subject: n.actionConfig.Config.DeclRange.Ptr(),
		})
	}
	return diags
}
