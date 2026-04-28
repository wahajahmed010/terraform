// Copyright IBM Corp. 2014, 2026
// SPDX-License-Identifier: BUSL-1.1

package terraform

import (
	"fmt"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs"
	"github.com/hashicorp/terraform/internal/plans"
)

// ActionDiffTransformer is a GraphTransformer that adds graph nodes representing
// each of the resource changes described in the given Changes object.
type ActionDiffTransformer struct {
	Changes *plans.ChangesSrc
	Config  *configs.Config
}

func (t *ActionDiffTransformer) Transform(g *Graph) error {
	// FIXME: remove dependency on hard-coded node types
	resourceInstanceNodes := addrs.MakeMap[addrs.AbsResourceInstance, []GraphNodeResourceInstance]()
	actionConfigNodes := addrs.MakeMap[addrs.ConfigAction, *NodeActionConfig]()

	// collect all the instance nodes, any of which could have action triggers
	for _, v := range g.Vertices() {
		switch v := v.(type) {
		case GraphNodeResourceInstance:
			instances := resourceInstanceNodes.Get(v.ResourceInstanceAddr())
			resourceInstanceNodes.Put(v.ResourceInstanceAddr(), append(instances, v))
		case *NodeActionConfig:
			actionConfigNodes.Put(v.ActionAddr(), v)
		}
	}

	for _, ai := range t.Changes.ActionInvocations {
		lat, ok := ai.ActionTrigger.(*plans.ResourceActionTrigger)
		if !ok {
			continue
		}

		atns, ok := resourceInstanceNodes.GetOk(lat.TriggeringResourceAddr)
		if !ok {
			return fmt.Errorf("no resource node found for action trigger %s", lat.TriggeringResourceAddr)
		}

		destroy := lat.ActionTriggerEvent == configs.EventBeforeDestroy || lat.ActionTriggerEvent == configs.EventAfterDestroy
		foundNode := false

		actionConfig, ok := actionConfigNodes.GetOk(ai.Addr.ConfigAction())
		if !ok {
			return fmt.Errorf("no action config node found for action trigger %s", lat.TriggeringResourceAddr)
		}

		// Add the action triggers to their instance nodes.
		for _, atn := range atns {
			if destroy {
				if n, ok := atn.(*NodeDestroyResourceInstance); ok {
					// FIXME: this doesn't deal with deposed or forget instances
					n.actionTriggers = append(n.actionTriggers, &nodeActionTriggerApplyInstance{
						ActionInvocation: ai,
						resolvedProvider: ai.ProviderAddr,
						actionConfig:     actionConfig,
					})
					foundNode = true

				}
				continue
			}

			if n, ok := atn.(*NodeApplyableResourceInstance); ok {
				n.actionTriggers = append(n.actionTriggers, &nodeActionTriggerApplyInstance{
					ActionInvocation: ai,
					resolvedProvider: ai.ProviderAddr,
					actionConfig:     actionConfig,
				})
				foundNode = true
			}
		}
		if !foundNode {
			return fmt.Errorf("no resource node found for action trigger %s", lat.TriggeringResourceAddr)
		}
	}

	return nil
}
