package main

import "strings"

// CommandHandler processes one inbound slash command.
type CommandHandler func(ev InboundEvent)

// commandRouter maps "/name" commands to centrally registered handlers.
// Adapters emit command events without interpreting them; this router is the
// single place that decides what a command means.
type commandRouter struct {
	handlers map[string]CommandHandler
}

func newCommandRouter() *commandRouter {
	return &commandRouter{handlers: make(map[string]CommandHandler)}
}

func (r *commandRouter) register(name string, h CommandHandler) {
	r.handlers[name] = h
}

// route dispatches the event; it reports false for unregistered commands so
// the caller can answer with the unknown-command notice.
func (r *commandRouter) route(ev InboundEvent) bool {
	h, ok := r.handlers[ev.Command]
	if !ok {
		return false
	}
	h(ev)
	return true
}

// InteractionHandler processes one inbound UI callback (button press).
type InteractionHandler func(ev InboundEvent)

type interactionBinding struct {
	prefix  string
	handler InteractionHandler
}

// interactionRouter dispatches callbacks by action prefix. Adapters forward
// every callback; the router owns the mapping from callback payload prefixes
// to feature owners and answers anything nobody claims.
type interactionRouter struct {
	bindings []interactionBinding
	fallback InteractionHandler
}

func newInteractionRouter() *interactionRouter {
	return &interactionRouter{}
}

func (r *interactionRouter) registerPrefix(prefix string, h InteractionHandler) {
	r.bindings = append(r.bindings, interactionBinding{prefix: prefix, handler: h})
}

func (r *interactionRouter) route(ev InboundEvent) {
	for _, b := range r.bindings {
		if strings.HasPrefix(ev.CallbackData, b.prefix) {
			b.handler(ev)
			return
		}
	}
	if r.fallback != nil {
		r.fallback(ev)
	}
}
