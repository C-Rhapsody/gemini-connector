package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

var errNoActiveMessengers = errors.New("no active messengers configured")

type bootstrapError struct {
	platform string
	reason   string
	err      error
}

func (e bootstrapError) Error() string {
	return e.platform + " adapter " + e.reason + " error: " + e.err.Error()
}

// BootstrapOptions carries the CLI-provided knobs into the composition root.
type BootstrapOptions struct {
	EnvFlag       string
	TelegramProxy string
	CronDisabled  bool
}

// Bootstrap is the composition root: it loads configuration, builds concrete
// adapters, wires the central controller, the turn coordinator and feature
// modules, and returns the running Application. It contains no business
// logic — only dependency assembly.
func Bootstrap(opts BootstrapOptions) (*Application, error) {
	exePath, _ := os.Executable()
	srcDir := filepath.Join(filepath.Dir(exePath), "..", "src")

	msgs, err := loadMessages(srcDir)
	if err != nil {
		log.Printf("Failed to load custom messages, using defaults: %v", err)
	}

	cfg, err := loadConfig(opts.EnvFlag)
	if err != nil {
		return nil, err
	}

	// Restore a persisted quota cooldown across restarts.
	if rem := RestoreQuotaCooldown(); rem > 0 {
		log.Printf("Quota cooldown restored from disk: %s remaining", formatQuotaDuration(rem))
	}

	if cfg.AgyConversationID == "" {
		log.Println("=========================================================")
		log.Println("WARNING: AGY_CONVERSATION_ID is missing in .env")
		log.Println("The bot will run, but it will NOT be able to trigger AI.")
		log.Println("Run 'agy' interactively once to authenticate, then restart the bot.")
		log.Println("=========================================================")
	} else {
		log.Printf("Target Antigravity Conversation ID: %s", cfg.AgyConversationID)
	}

	// Build adapters based on ACTIVE_MESSENGERS.
	adapters := make(map[string]Messenger)
	var listenChannels []<-chan InboundEvent

	for _, name := range cfg.ActiveMessengers {
		switch name {
		case "telegram":
			adapters["telegram"] = NewTelegramAdapter(cfg.TelegramBotToken, cfg.TelegramChatID, msgs, cfg.ConversationID, opts.TelegramProxy)
		case "teams":
			adapters["teams"] = NewTeamsAdapter(cfg.TeamsTenantID, cfg.TeamsAppID, cfg.TeamsAppSecret, cfg.TeamsChatID, msgs)
		default:
			log.Printf("Unknown messenger: %s (skipped)", name)
		}
	}

	if len(adapters) == 0 {
		return nil, errNoActiveMessengers
	}

	for name, adapter := range adapters {
		if err := adapter.Init(); err != nil {
			return nil, bootstrapError{platform: name, reason: "init", err: err}
		}
		ch, err := adapter.Listen()
		if err != nil {
			return nil, bootstrapError{platform: name, reason: "listen", err: err}
		}
		listenChannels = append(listenChannels, ch)
		log.Printf("Adapter [%s] started.", name)
	}

	// Send startup welcome to each adapter's configured chat.
	if tg, ok := adapters["telegram"]; ok && cfg.TelegramChatID != 0 {
		tg.Send(strconv.FormatInt(cfg.TelegramChatID, 10), msgs.StartupWelcome)
	}
	if teams, ok := adapters["teams"]; ok {
		teams.Send(cfg.TeamsChatID, msgs.StartupWelcome)
	}

	registry := NewAdapterRegistry(adapters)
	turns := NewTurnCoordinator()

	var cronSurface CronSurface
	if cs, ok := adapters["telegram"].(CronSurface); ok {
		cronSurface = cs
	}
	cron, err := NewCronModule(CronModuleOptions{
		Disabled:       opts.CronDisabled,
		AdminUserIDs:   cfg.CronAdminTelegramUserIDs,
		ConvID:         cfg.ConversationID,
		MissingConvMsg: defaultMessages.ErrorMissingUUID,
		Surface:        cronSurface,
		Turns:          turns,
	})
	if err != nil {
		return nil, err
	}

	var cronSvc *CronService
	if cron != nil {
		cronSvc = cron.Service
	}

	controller := NewController(registry, turns, cfg, msgs, cronSvc)

	return &Application{
		cfg:        cfg,
		msgs:       msgs,
		registry:   registry,
		events:     fanIn(listenChannels...),
		controller: controller,
		turns:      turns,
		cron:       cron,
	}, nil
}
