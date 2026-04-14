// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/dag"
	"github.com/hashicorp/terraform/internal/lang/langrefs"
	"github.com/hashicorp/terraform/internal/providers"
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
