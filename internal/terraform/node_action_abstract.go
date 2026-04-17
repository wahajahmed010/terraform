// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/lang/langrefs"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// NodeActionConfig represents an action in the configuration. This node is
// primarily concerned with resolving provider references and receiving the
// correct schema. All expansion and execution is done from an action trigger.
type NodeActionConfig struct {
	Addr   addrs.ConfigAction
	Config configs.Action

	// The fields below will be automatically set using the Attach interfaces if
	// you're running those transforms, but also can be explicitly set if you
	// already have that information.

	// The address of the provider this action will use
	ResolvedProvider addrs.AbsProviderConfig
	Schema           *providers.ActionSchema
	Dependencies     []addrs.ConfigResource

	// TODO: cache evaluations?
}

var (
	_ GraphNodeReferenceable      = (*NodeActionConfig)(nil)
	_ GraphNodeReferencer         = (*NodeActionConfig)(nil)
	_ GraphNodeConfigAction       = (*NodeActionConfig)(nil)
	_ GraphNodeAttachActionSchema = (*NodeActionConfig)(nil)
	_ GraphNodeProviderConsumer   = (*NodeActionConfig)(nil)
	_ GraphNodeAttachDependencies = (*NodeActionConfig)(nil)
)

func (n NodeActionConfig) Name() string {
	return n.Addr.String()
}

// The action config does not expand or execute itself during plan or apply, but
// for Validate it does verify valid configuration.
func (n *NodeActionConfig) Execute(ctx EvalContext, op walkOperation) tfdiags.Diagnostics {
	if op != walkValidate {
		return nil
	}
	return n.validate(ctx)
}

func (n *NodeActionConfig) validate(ctx EvalContext) tfdiags.Diagnostics {

	var diags tfdiags.Diagnostics
	// FIXME: count/for_each validation

	if n.Config.Config == nil {
		return nil
	}

	repData := EvalDataForNoInstanceKey

	switch {
	case n.Config.Count != nil:
		// If the config block has count, we'll evaluate with an unknown
		// number as count.index so we can still type check even though
		// we won't expand count until the plan phase.
		repData = InstanceKeyEvalData{
			CountIndex: cty.UnknownVal(cty.Number),
		}

		// Basic type-checking of the count argument. More complete validation
		// of this will happen when we DynamicExpand during the plan walk.
		_, countDiags := evaluateCountExpressionValue(n.Config.Count, ctx)
		diags = diags.Append(countDiags)

	case n.Config.ForEach != nil:
		repData = InstanceKeyEvalData{
			EachKey:   cty.UnknownVal(cty.String),
			EachValue: cty.UnknownVal(cty.DynamicPseudoType),
		}

		// Evaluate the for_each expression here so we can expose the diagnostics
		forEachDiags := newForEachEvaluator(n.Config.ForEach, ctx, false).ValidateResourceValue()
		diags = diags.Append(forEachDiags)
	}

	_, valDiags := n.evalInstance(ctx, repData)
	diags = diags.Append(valDiags)
	if valDiags.HasErrors() {
		return diags
	}
	return diags
}

// ConcreteActionNodeFunc is a callback type used to convert an
// abstract action to a concrete one of some type.
type ConcreteActionNodeFunc func(*NodeActionConfig) dag.Vertex

// DefaultConcreteActionNodeFunc is the default ConcreteActionNodeFunc used by
// everything except validate.
func DefaultConcreteActionNodeFunc(a *NodeActionConfig) dag.Vertex {
	return &nodeExpandAction{
		NodeActionConfig: a,
	}
}

// GraphNodeConfigAction
func (n NodeActionConfig) ActionAddr() addrs.ConfigAction {
	return n.Addr
}

func (n NodeActionConfig) ModulePath() addrs.Module {
	return n.Addr.Module
}

func (n *NodeActionConfig) Path() addrs.ModuleInstance {
	// this node is only directly evaluated during validation, so there is never
	// module expansion.
	return n.Addr.Module.UnkeyedInstanceShim()
}

func (n *NodeActionConfig) ReferenceableAddrs() []addrs.Referenceable {
	return []addrs.Referenceable{n.Addr.Action}
}

func (n *NodeActionConfig) References() []*addrs.Reference {
	var result []*addrs.Reference
	c := n.Config

	refs, _ := langrefs.ReferencesInExpr(addrs.ParseRef, c.Count)
	result = append(result, refs...)
	refs, _ = langrefs.ReferencesInExpr(addrs.ParseRef, c.ForEach)
	result = append(result, refs...)

	if n.Schema != nil {
		refs, _ = langrefs.ReferencesInBlock(addrs.ParseRef, c.Config, n.Schema.ConfigSchema)
		result = append(result, refs...)
	}

	return result
}

func (n *NodeActionConfig) AttachActionSchema(schema *providers.ActionSchema) {
	n.Schema = schema
}

func (n *NodeActionConfig) Provider() ProviderRef {
	// If the resolvedProvider is set, use that
	if n.ResolvedProvider.Provider.Type != "" {
		ref := ProviderRef{
			Addr:     n.ResolvedProvider,
			Resolved: true,
		}
		return ref
	}

	var addr addrs.AbsProviderConfig
	if n.Config.Provider.Type != "" {
		addr.Provider = n.Config.Provider
	} else {
		addr.Provider = addrs.ImpliedProviderForUnqualifiedType(n.Addr.Action.ImpliedProvider())
	}

	addr.Alias = n.Config.ProviderConfigAddr().Alias
	addr.Module = n.ModulePath()
	return ProviderRef{
		Addr: addr,
	}
}

func (n *NodeActionConfig) SetProvider(p addrs.AbsProviderConfig) {
	n.ResolvedProvider = p
}

func (n *NodeActionConfig) AttachDependencies(deps []addrs.ConfigResource) {
	n.Dependencies = deps
}

// FIXME: unknown repdata deferrals
func (n *NodeActionConfig) repetitionData(ctx EvalContext) ([]instances.RepetitionData, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var reps []instances.RepetitionData

	switch {
	case n.Config.Count != nil:
		count, countDiags := evaluateCountExpression(n.Config.Count, ctx, false)
		diags = diags.Append(countDiags)
		if diags.HasErrors() {
			return nil, diags
		}

		for i := 0; i < count; i++ {
			reps = append(reps, instances.RepetitionData{
				CountIndex: cty.NumberIntVal(int64(i)),
			})
		}

		return reps, diags

	case n.Config.ForEach != nil:
		forEach, _, forEachDiags := evaluateForEachExpression(n.Config.ForEach, ctx, false)
		diags = diags.Append(forEachDiags)
		if forEachDiags.HasErrors() {
			return reps, diags
		}

		for key, value := range forEach {
			reps = append(reps, instances.RepetitionData{
				EachKey:   cty.StringVal(key),
				EachValue: value,
			})
		}
		return reps, diags

	default:
		return nil, diags
	}
}

// Eval returns the value of the expanded config block.
func (n *NodeActionConfig) Eval(ctx EvalContext) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// This should have been caught already
	if n.Schema == nil {
		panic("action eval called without a schema")
	}

	actionInstances, diags := n.repetitionData(ctx)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}

	switch {
	case n.Config.Count != nil:
		var vals []cty.Value
		for _, inst := range actionInstances {
			val, evalDiags := n.evalInstance(ctx, inst)
			diags = diags.Append(evalDiags)
			if evalDiags.HasErrors() {
				return cty.DynamicVal, diags
			}

			vals = append(vals, val)
		}
		return cty.TupleVal(vals), diags

	case n.Config.ForEach != nil:
		vals := make(map[string]cty.Value)
		for _, inst := range actionInstances {
			val, evalDiags := n.evalInstance(ctx, inst)
			diags = diags.Append(evalDiags)
			if evalDiags.HasErrors() {
				return cty.DynamicVal, diags
			}

			vals[inst.EachKey.AsString()] = val
		}

		return cty.ObjectVal(vals), diags

	default:
		return n.evalInstance(ctx, instances.RepetitionData{})
	}
}

func (n *NodeActionConfig) evalInstance(ctx EvalContext, repData instances.RepetitionData) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	configVal := cty.NullVal(n.Schema.ConfigSchema.ImpliedType())
	if n.Config.Config != nil {
		var configDiags tfdiags.Diagnostics
		configVal, _, configDiags = ctx.EvaluateBlock(n.Config.Config, n.Schema.ConfigSchema.DeepCopy(), nil, repData)
		diags = diags.Append(configDiags)
		if configDiags.HasErrors() {
			return configVal, diags
		}

		valDiags := validateResourceForbiddenEphemeralValues(ctx, configVal, n.Schema.ConfigSchema)
		diags = diags.Append(valDiags.InConfigBody(n.Config.Config, n.Addr.String()))

		var deprecationDiags tfdiags.Diagnostics
		configVal, deprecationDiags = ctx.Deprecations().ValidateAndUnmarkConfig(configVal, n.Schema.ConfigSchema, n.ModulePath())
		diags = diags.Append(deprecationDiags.InConfigBody(n.Config.Config, n.Addr.String()))
	}
	return configVal, diags
}
