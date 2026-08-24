package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// Controller is the central inbound routing layer: it classifies adapter
// events and hands each one to the owning feature — command router,
// interaction router or the serialized agy turn path. It is the only place
// that decides what a command or callback means; adapters merely translate.
type Controller struct {
	registry     *AdapterRegistry
	turns        *TurnCoordinator
	cfg          *Config
	msgs         *Messages
	cron         *CronService
	commands     *commandRouter
	interactions *interactionRouter
}

func NewController(registry *AdapterRegistry, turns *TurnCoordinator, cfg *Config, msgs *Messages, cron *CronService) *Controller {
	if msgs != nil {
		msgs.applyDefaults()
	}
	c := &Controller{
		registry:     registry,
		turns:        turns,
		cfg:          cfg,
		msgs:         msgs,
		cron:         cron,
		commands:     newCommandRouter(),
		interactions: newInteractionRouter(),
	}
	c.registerHandlers()
	return c
}

func (c *Controller) registerHandlers() {
	for _, name := range []string{"/start", "/help"} {
		c.commands.register(name, c.handleHelp)
	}
	c.commands.register("/stop", c.handleStop)
	c.commands.register("/reset", c.handleReset)
	c.commands.register("/clear", c.handleClear)
	c.commands.register("/image", c.handleImage)
	c.commands.register("/switch", c.handleSwitch)
	c.commands.register("/list", c.handleList)
	c.commands.register("/status", c.handleStatus)
	c.commands.register("/summary", c.handleSummary)
	c.commands.register("/version", c.handleVersion)
	c.commands.register("/cron", c.handleCron)

	if c.cron != nil {
		c.interactions.registerPrefix(cronCbConfirm, c.cron.HandleCallback)
		c.interactions.registerPrefix(cronCbCancel, c.cron.HandleCallback)
		c.interactions.registerPrefix(cronCbSelectRef, c.cron.HandleCallback)
	}
	c.interactions.fallback = c.handleUnknownInteraction
}

// Handle routes one inbound event. It is invoked on its own goroutine per
// event, so handlers may block (e.g. the cron planner awaiting its queued
// turn) without stalling the event loop.
func (c *Controller) Handle(ev InboundEvent) {
	switch ev.RouteKind() {
	case EventInteraction:
		c.interactions.route(ev)
	case EventCommand:
		if !c.commands.route(ev) {
			if adapter, ok := c.registry.Get(ev.Platform); ok {
				adapter.Send(ev.ChatID, c.msgs.CommandUnknown, SendOptions{ReplyToMessageID: ev.MessageID})
			}
		}
	default:
		adapter, ok := c.registry.Get(ev.Platform)
		if !ok {
			log.Printf("No adapter for platform: %s", ev.Platform)
			return
		}
		c.handleChat(adapter, ev)
	}
}

func (c *Controller) replyOpt(ev InboundEvent) SendOptions {
	return SendOptions{ReplyToMessageID: ev.MessageID}
}

func (c *Controller) send(ev InboundEvent, text string, opts ...SendOptions) {
	if adapter, ok := c.registry.Get(ev.Platform); ok {
		adapter.Send(ev.ChatID, text, opts...)
	}
}

// --- Command handlers ---

func (c *Controller) handleHelp(ev InboundEvent) {
	c.send(ev, c.msgs.CommandStartHelp, c.replyOpt(ev))
}

func (c *Controller) handleStop(ev InboundEvent) {
	active, dropped := c.turns.StopActive()
	switch {
	case active && dropped > 0:
		c.send(ev, fmt.Sprintf(c.msgs.StopDoneWithQueued, dropped), c.replyOpt(ev))
	case active:
		c.send(ev, c.msgs.StopDone, c.replyOpt(ev))
	default:
		c.send(ev, c.msgs.StopNothing, c.replyOpt(ev))
	}
}

func (c *Controller) handleReset(ev InboundEvent) {
	c.submitQueued(ev, func(ctx context.Context) {
		resetConversation(ctx, c.cfg, c.adapterFor(ev), ev.ChatID, ev.MessageID, "", c.msgs)
	})
}

func (c *Controller) handleClear(ev InboundEvent) {
	c.submitQueued(ev, func(ctx context.Context) {
		clearConversation(ctx, c.cfg, c.adapterFor(ev), ev.ChatID, ev.MessageID, c.msgs)
	})
}

func (c *Controller) handleImage(ev InboundEvent) {
	c.submitQueued(ev, func(ctx context.Context) {
		imageCommand(ctx, c.cfg, c.adapterFor(ev), ev.ChatID, ev.MessageID, ev.Args, c.msgs)
	})
}

func (c *Controller) handleSwitch(ev InboundEvent) {
	c.submitQueued(ev, func(ctx context.Context) {
		switchConversation(c.cfg, c.adapterFor(ev), ev.ChatID, ev.Args, ev.MessageID, c.msgs)
	})
}

func (c *Controller) handleList(ev InboundEvent) {
	listConversations(c.cfg, c.adapterFor(ev), ev.ChatID, ev.Args, ev.MessageID, c.msgs)
}

func (c *Controller) handleStatus(ev InboundEvent) {
	statusConversation(c.cfg, c.adapterFor(ev), ev.ChatID, ev.MessageID, c.msgs)
}

func (c *Controller) handleSummary(ev InboundEvent) {
	summaryConversation(c.cfg, c.adapterFor(ev), ev.ChatID, ev.MessageID, c.msgs)
}

func (c *Controller) handleVersion(ev InboundEvent) {
	versionInfo(c.adapterFor(ev), ev.ChatID, ev.MessageID, c.msgs)
}

func (c *Controller) handleCron(ev InboundEvent) {
	if c.cron == nil {
		c.send(ev, cronDisabledNotice, c.replyOpt(ev))
		return
	}
	c.cron.HandleMessage(ev)
}

func (c *Controller) handleUnknownInteraction(ev InboundEvent) {
	if ui := c.registry.Interactive(ev.Platform); ui != nil {
		ui.AnswerCallbackQuery(ev.CallbackID, "")
	}
}

// --- Serialized turn path ---

func (c *Controller) submitQueued(ev InboundEvent, run func(ctx context.Context)) {
	ahead := c.turns.Submit(run)
	if ahead > 0 {
		c.send(ev, fmt.Sprintf(c.msgs.QueuedNotice, ahead), c.replyOpt(ev))
	}
}

func (c *Controller) adapterFor(ev InboundEvent) Messenger {
	adapter, _ := c.registry.Get(ev.Platform)
	return adapter
}

// handleChat submits a regular user message as a serialized agy turn.
func (c *Controller) handleChat(adapter Messenger, ev InboundEvent) {
	replyOpt := c.replyOpt(ev)

	if c.cfg.ConversationID() == "" {
		adapter.Send(ev.ChatID, c.msgs.ErrorMissingUUID, replyOpt)
		return
	}

	// Fast path during quota cooldown: reply immediately without touching
	// the turn queue. Jobs that are already queued get the same treatment
	// inside executeAgy.
	if QuotaActive() {
		adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
		return
	}

	ahead := c.turns.Submit(func(ctx context.Context) {
		c.runChatTurn(ctx, adapter, ev)
	})
	if ahead > 0 {
		adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.QueuedNotice, ahead), replyOpt)
	}
}

// runChatTurn executes one agy turn end-to-end: typing state, CLI call,
// error classification with self-healing, transcript bookkeeping and result
// delivery.
func (c *Controller) runChatTurn(ctx context.Context, adapter Messenger, ev InboundEvent) {
	replyOpt := c.replyOpt(ev)

	stop := adapter.StartTyping(ev.ChatID)
	defer stop()

	// Turn start marker: files modified from this point on are considered
	// AI-produced and eligible for attachment delivery.
	turnStart := time.Now()

	response, err := agyInvoker(ctx, ev.Content, c.cfg.ConversationID(), AgyCallOptions{})
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled via /stop: stay silent, the stop notice already went out.
			log.Printf("agy turn cancelled by /stop: %s", truncateString(ev.Content, 50))
			return
		}
		c.deliverTurnError(ctx, adapter, ev, err)
		return
	}

	QuotaClear()

	if ctx.Err() != nil {
		log.Printf("agy turn cancelled by /stop (after completion): %s", truncateString(ev.Content, 50))
		return
	}
	if response != "" {
		appendTranscript(c.cfg.ConversationID(), "user", ev.Content)
		appendTranscript(c.cfg.ConversationID(), "assistant", response)
		adapter.Send(ev.ChatID, response, SendOptions{ReplyToMessageID: ev.MessageID, AttachAfter: turnStart})
	} else {
		adapter.Send(ev.ChatID, c.msgs.ErrorEmptyResponse, replyOpt)
	}
}

// deliverTurnError maps an agy failure onto user-visible replies and the
// stuck-conversation self-healing rules.
func (c *Controller) deliverTurnError(ctx context.Context, adapter Messenger, ev InboundEvent, err error) {
	replyOpt := c.replyOpt(ev)
	ae, ok := err.(*AgyError)
	if !ok {
		return
	}
	switch ae.Type {
	case "cli_failure":
		adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorCLIFailure, ae.Err, ae.Detail), replyOpt)
	case "json_parse_fail":
		adapter.Send(ev.ChatID, c.msgs.ErrorJSONParseFail, replyOpt)
	case "quota_cooldown":
		adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorSystemResponse, ae.Detail), replyOpt)
	case "error_status":
		detail := ae.Detail
		// 429 quota exhaustion: start/refresh cooldown and answer with the
		// decremented error text.
		if QuotaCapture(detail) {
			adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorSystemResponse, QuotaRefreshedDetail()), replyOpt)
			return
		}
		if extractUrlFetchFailure(detail) != "" && c.cfg.recordStuckError("url_fetch", detail) {
			log.Printf("Stuck conversation detected (repeated URL fetch failure), resetting session")
			resetConversation(ctx, c.cfg, adapter, ev.ChatID, ev.MessageID, ev.Content, c.msgs)
			return
		}
		// Conversation-state validation failures poison every subsequent
		// turn; auto-reset after a repeat.
		if strings.Contains(detail, "invalid arguments") {
			if c.cfg.recordStuckError("invalid_args", detail) {
				log.Printf("Repeated invalid-arguments errors, auto-resetting session")
				resetConversation(ctx, c.cfg, adapter, ev.ChatID, ev.MessageID, ev.Content, c.msgs)
				return
			}
			adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorSystemResponse, detail+invalidArgsHint), replyOpt)
			return
		}
		// Any other repeating error_status: fall back to a fresh session
		// WITHOUT replaying the failing prompt — such errors are often
		// environmental or model-behavioural, so replaying would just burn
		// quota in a loop.
		if c.cfg.recordStuckError("generic", detail) {
			log.Printf("Repeated error detected, auto-resetting session without replay")
			resetConversation(ctx, c.cfg, adapter, ev.ChatID, ev.MessageID, "", c.msgs)
			return
		}
		adapter.Send(ev.ChatID, fmt.Sprintf(c.msgs.ErrorSystemResponse, detail), replyOpt)
	case "authentication_required":
		adapter.Send(ev.ChatID, "⚠️ agy 인증이 필요합니다. 터미널에서 'agy'를 한 번 실행해 인증을 완료한 뒤 봇을 재시작하세요.", replyOpt)
	}
}
