package llm

import (
	"github.com/daknoblo/ai-ui/internal/foundry"
)

func (c *Client) route(op foundry.Operation, pin string) (*Client, foundry.Deployment, error) {
	store, deployment, err := c.store.SelectRoute(op, pin)
	if err != nil {
		return nil, deployment, err
	}
	bound := *c
	bound.store = store
	return &bound, deployment, nil
}

// PrepareChat freezes a single provider across tool continuations and parameter
// negotiation. The private route is deliberately not serialized into durable jobs.
func (c *Client) PrepareChat(opts ChatOptions, vision bool) (ChatOptions, error) {
	if opts.route != nil || !c.store.Get().Foundry {
		return opts, nil
	}

	op := foundry.Chat
	if vision {
		op = foundry.Vision
		if opts.Model != "" {
			if _, err := c.store.ResolveDeployment(op, opts.Model); err != nil {
				opts.Model = ""
			}
		}
	}
	bound, deployment, err := c.route(op, opts.Model)
	if err != nil {
		return opts, err
	}
	opts.route = bound
	opts.Model = deployment.Name
	opts.ReasoningEffort = NormalizeReasoningEffort(deployment.ModelName, opts.ReasoningEffort)
	return opts, nil
}

func (opts ChatOptions) SupportsTools() bool {
	if opts.route == nil {
		return true
	}
	deployment, err := opts.route.store.ResolveDeployment(foundry.Chat, opts.Model)
	return err == nil && deployment.SupportsTools()
}
