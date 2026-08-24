package main

import (
	"log"
	"sync"
)

// fanIn merges multiple inbound event channels into one.
func fanIn(channels ...<-chan InboundEvent) <-chan InboundEvent {
	merged := make(chan InboundEvent, 100)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(c <-chan InboundEvent) {
			defer wg.Done()
			for ev := range c {
				merged <- ev
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(merged)
	}()
	return merged
}

// Application owns the running connector: adapters, the central controller,
// the turn coordinator and optional feature modules. main.go assembles it via
// Bootstrap and then only waits for shutdown; every runtime decision lives in
// here or below.
type Application struct {
	cfg        *Config
	msgs       *Messages
	registry   *AdapterRegistry
	events     <-chan InboundEvent
	controller *Controller
	turns      *TurnCoordinator
	cron       *CronModule
}

// Run consumes adapter events until stop closes or the event stream ends,
// then unwinds feature modules. It blocks.
func (a *Application) Run(stop <-chan struct{}) {
	log.Println("Waiting for messages...")

loop:
	for running := true; running; {
		select {
		case <-stop:
			running = false
		case ev, ok := <-a.events:
			if !ok {
				break loop
			}
			go a.controller.Handle(ev)
		}
	}

	log.Println("Shutting down connector...")
	a.cron.Stop()
	if err := a.cron.Close(); err != nil {
		log.Printf("Cron store close error: %v", err)
	}
}
